package querystaking

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/certusone/wormhole/node/pkg/query/queryratelimit"
	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/zap"
)

// QueryType represents the different types of queries supported
type QueryType uint8

// Query type constants
const (
	EthCallQueryRequestType             QueryType = 1
	EthCallByTimestampQueryRequestType  QueryType = 2
	EthCallWithFinalityQueryRequestType QueryType = 3
	SolanaAccountQueryRequestType       QueryType = 4
	SolanaPdaQueryRequestType           QueryType = 5
)

// Metrics specific to staking module
var (
	stakingPolicyFetches = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ccq_staking_policy_fetches_total",
			Help: "Total number of staking policy fetches by result",
		}, []string{"result", "pool"})

	stakingQueryLatency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "ccq_staking_query_duration_seconds",
			Help:    "Staking contract query latency by pool and operation",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0},
		}, []string{"pool", "operation"})

	stakingPolicyDecisions = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ccq_staking_policy_decisions_total",
			Help: "Policy decisions made by staking provider by outcome and tier",
		}, []string{"outcome", "tier", "query_type"})

	stakingPoolErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ccq_staking_pool_errors_total",
			Help: "Errors when querying staking pools by pool and error type",
		}, []string{"pool", "error_type"})
)

// QueryTypePool defines a query type pool that this node supports
// Rate limiting configuration (tranches/rates) is determined by the conversion table
// stored on-chain, not hardcoded here.
type QueryTypePool struct {
	QueryTypes []QueryType // Query types included in this pool
}

// QueryTypeBits generates the bit field from the QueryTypes slice
func (p QueryTypePool) QueryTypeBits() [32]byte {
	var bits [32]byte
	for _, qt := range p.QueryTypes {
		if qt == 0 {
			continue
		}
		qtUint8 := uint8(qt)
		if qtUint8 > 255 {
			continue
		}
		byteIndex := 31 - (qtUint8-1)/8
		bitOffset := (qtUint8 - 1) % 8
		bits[byteIndex] |= 1 << bitOffset
	}
	return bits
}

// SupportedQueryPools defines the query type pools that this node supports
// Pools are discovered via the factory contract using the query type bits.
// Rate limiting (tranches/rates) is determined by each pool's conversion table.
var SupportedQueryPools = map[string]QueryTypePool{
	"evm": {
		QueryTypes: []QueryType{
			EthCallQueryRequestType,
			EthCallByTimestampQueryRequestType,
			EthCallWithFinalityQueryRequestType,
		},
	},
	"solana": {
		QueryTypes: []QueryType{
			SolanaAccountQueryRequestType,
			SolanaPdaQueryRequestType,
		},
	},
}

// QueryTypeToChain maps query types to their chain name for IPFS data lookup
// This mapping is used to extract chain-specific rates from conversion tables
var QueryTypeToChain = map[QueryType]string{
	EthCallQueryRequestType:             "EVM",
	EthCallByTimestampQueryRequestType:  "EVM",
	EthCallWithFinalityQueryRequestType: "EVM",
	SolanaAccountQueryRequestType:       "Solana",
	SolanaPdaQueryRequestType:           "Solana",
}

// GetChainName returns the chain name for a query type
func GetChainName(qt QueryType) (string, error) {
	chainName, ok := QueryTypeToChain[qt]
	if !ok {
		return "", fmt.Errorf("unknown query type: %d", qt)
	}
	return chainName, nil
}

// PoolMetadata holds immutable pool data that can be safely cached.
// These values are set at pool deployment and never change.
type PoolMetadata struct {
	StakingTokenAddress common.Address
	TokenDecimals       uint8
}

// StakingClient wraps ethereum client for staking contract interactions
type StakingClient struct {
	client         *ethclient.Client
	logger         *zap.Logger
	factoryAddress common.Address
	ipfsClient     *IPFSClient

	// Single mutex protects all caches (accessed sequentially in FetchStakingPolicy)
	cacheMutex sync.RWMutex

	// Caches for immutable contract data
	conversionHistoryCache map[common.Address][][32]byte   // Pool -> CID array
	poolMetadataCache      map[common.Address]*PoolMetadata // Pool -> token metadata
	factoryPoolCache       map[[32]byte]common.Address      // QueryTypeBits -> pool address
}

// NewStakingClient creates a new staking client
func NewStakingClient(client *ethclient.Client, logger *zap.Logger, factoryAddress common.Address, ipfsClient *IPFSClient) *StakingClient {
	return &StakingClient{
		client:                 client,
		logger:                 logger.With(zap.String("component", "staking-client")),
		factoryAddress:         factoryAddress,
		ipfsClient:             ipfsClient,
		conversionHistoryCache: make(map[common.Address][][32]byte),
		poolMetadataCache:      make(map[common.Address]*PoolMetadata),
		factoryPoolCache:       make(map[[32]byte]common.Address),
	}
}

// GetStakeInfo queries a pool for a staker's information with comprehensive error handling
func (sc *StakingClient) GetStakeInfo(ctx context.Context, poolAddress, stakerAddress common.Address, poolName string) (*StakeInfo, error) {
	start := time.Now()

	sc.logger.Debug("querying stake info",
		zap.String("pool", poolName),
		zap.String("poolAddress", poolAddress.Hex()),
		zap.String("staker", stakerAddress.Hex()))

	callData := PackStakesCall(stakerAddress)

	// Measure RPC call latency
	rpcStart := time.Now()
	result, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddress,
		Data: callData,
	}, nil)

	stakingQueryLatency.WithLabelValues(poolName, "rpc_call").Observe(time.Since(rpcStart).Seconds())

	if err != nil {
		stakingPoolErrors.WithLabelValues(poolName, "rpc_error").Inc()

		sc.logger.Error("failed to call staking contract",
			zap.String("pool", poolName),
			zap.String("poolAddress", poolAddress.Hex()),
			zap.String("staker", stakerAddress.Hex()),
			zap.Error(err))

		return nil, fmt.Errorf("failed to call stakes on pool %s: %w", poolName, err)
	}

	// Measure parsing latency
	parseStart := time.Now()
	stakeInfo, err := ParseStakeInfo(result)
	stakingQueryLatency.WithLabelValues(poolName, "parse_result").Observe(time.Since(parseStart).Seconds())

	if err != nil {
		stakingPoolErrors.WithLabelValues(poolName, "parse_error").Inc()
		sc.logger.Error("failed to parse stake info",
			zap.String("pool", poolName),
			zap.String("staker", stakerAddress.Hex()),
			zap.Int("resultLength", len(result)),
			zap.Error(err))
		return nil, fmt.Errorf("failed to parse stake info from pool %s: %w", poolName, err)
	}

	// Log successful query with stake details
	sc.logger.Debug("successfully queried stake info",
		zap.String("pool", poolName),
		zap.String("staker", stakerAddress.Hex()),
		zap.String("stakeAmount", stakeInfo.Amount.String()),
		zap.Uint64("lockupEnd", stakeInfo.LockupEnd),
		zap.Uint64("accessEnd", stakeInfo.AccessEnd),
		zap.Duration("totalLatency", time.Since(start)))

	return stakeInfo, nil
}

// GetSignerAddress queries the stakerSigners mapping to find the designated signer for a staker
func (sc *StakingClient) GetSignerAddress(ctx context.Context, poolAddress, stakerAddress common.Address, poolName string) (common.Address, error) {
	callData := PackStakerSignersCall(stakerAddress)

	result, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddress,
		Data: callData,
	}, nil)
	if err != nil {
		sc.logger.Debug("failed to call stakerSigners",
			zap.String("pool", poolName),
			zap.String("staker", stakerAddress.Hex()),
			zap.Error(err))
		return common.Address{}, fmt.Errorf("failed to call stakerSigners on pool %s: %w", poolName, err)
	}

	signerAddress, err := ParseSignerAddress(result)
	if err != nil {
		sc.logger.Error("failed to parse signer address",
			zap.String("pool", poolName),
			zap.String("staker", stakerAddress.Hex()),
			zap.Error(err))
		return common.Address{}, fmt.Errorf("failed to parse signer address from pool %s: %w", poolName, err)
	}

	return signerAddress, nil
}

// VerifySignerAuthorization verifies that signerAddr is authorized to act on behalf of stakerAddr
// Returns nil if authorized, error otherwise
func (sc *StakingClient) VerifySignerAuthorization(ctx context.Context, poolAddress, stakerAddr, signerAddr common.Address, poolName string) error {
	// Case 1: Self-staking (signer is the staker)
	if stakerAddr == signerAddr {
		sc.logger.Debug("signer is staker (self-staking)",
			zap.String("pool", poolName),
			zap.String("address", stakerAddr.Hex()))
		return nil
	}

	// Case 2: Delegated signer - verify delegation
	delegatedSigner, err := sc.GetSignerAddress(ctx, poolAddress, stakerAddr, poolName)
	if err != nil {
		return fmt.Errorf("failed to get delegated signer for staker %s: %w", stakerAddr.Hex(), err)
	}

	// Check if the delegated signer matches
	if delegatedSigner != signerAddr {
		// Special case: if delegatedSigner is zero address, no delegation is set
		if delegatedSigner == (common.Address{}) {
			return fmt.Errorf("no signer delegated for staker %s, but signer %s attempted to act on their behalf",
				stakerAddr.Hex(), signerAddr.Hex())
		}
		return fmt.Errorf("signer %s is not authorized for staker %s (expected %s)",
			signerAddr.Hex(), stakerAddr.Hex(), delegatedSigner.Hex())
	}

	sc.logger.Debug("verified delegated signer authorization",
		zap.String("pool", poolName),
		zap.String("staker", stakerAddr.Hex()),
		zap.String("signer", signerAddr.Hex()))

	return nil
}

// IsBlocklisted checks if an address is blocklisted for a specific pool
func (sc *StakingClient) IsBlocklisted(ctx context.Context, poolAddress, userAddress common.Address, poolName string) (bool, error) {
	callData := PackIsBlocklistedCall(userAddress)

	result, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddress,
		Data: callData,
	}, nil)
	if err != nil {
		sc.logger.Debug("failed to call isBlocklisted",
			zap.String("pool", poolName),
			zap.String("user", userAddress.Hex()),
			zap.Error(err))
		return false, fmt.Errorf("failed to call isBlocklisted on pool %s: %w", poolName, err)
	}

	isBlocked, err := ParseBoolResult(result)
	if err != nil {
		sc.logger.Error("failed to parse blocklist result",
			zap.String("pool", poolName),
			zap.String("user", userAddress.Hex()),
			zap.Error(err))
		return false, fmt.Errorf("failed to parse blocklist result from pool %s: %w", poolName, err)
	}

	return isBlocked, nil
}

// GetPoolMetadata fetches and caches immutable pool metadata (staking token address and decimals).
// This eliminates repeated contract calls for data that never changes.
func (sc *StakingClient) GetPoolMetadata(ctx context.Context, poolAddress common.Address, poolName string) (*PoolMetadata, error) {
	// Check cache first (with read lock)
	sc.cacheMutex.RLock()
	cached, exists := sc.poolMetadataCache[poolAddress]
	sc.cacheMutex.RUnlock()

	if exists {
		sc.logger.Debug("using cached pool metadata",
			zap.String("pool", poolName),
			zap.String("poolAddress", poolAddress.Hex()),
			zap.Uint8("decimals", cached.TokenDecimals))
		return cached, nil
	}

	// Not in cache, acquire write lock and fetch
	sc.cacheMutex.Lock()
	defer sc.cacheMutex.Unlock()

	// Double-check cache in case another goroutine filled it while we waited
	if cached, exists := sc.poolMetadataCache[poolAddress]; exists {
		return cached, nil
	}

	sc.logger.Debug("fetching pool metadata from contract",
		zap.String("pool", poolName),
		zap.String("poolAddress", poolAddress.Hex()))

	// Get STAKING_TOKEN address from the pool
	tokenResult, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddress,
		Data: PackStakingTokenCall(),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get STAKING_TOKEN from pool %s: %w", poolName, err)
	}

	tokenAddress, err := ParseAddressResult(tokenResult)
	if err != nil {
		return nil, fmt.Errorf("failed to parse STAKING_TOKEN address from pool %s: %w", poolName, err)
	}

	// Get decimals from the token contract
	decimalsResult, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &tokenAddress,
		Data: PackDecimalsCall(),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get decimals from token %s: %w", tokenAddress.Hex(), err)
	}

	decimals, err := ParseUint8Result(decimalsResult)
	if err != nil {
		return nil, fmt.Errorf("failed to parse decimals from token %s: %w", tokenAddress.Hex(), err)
	}

	// Store in cache
	metadata := &PoolMetadata{
		StakingTokenAddress: tokenAddress,
		TokenDecimals:       decimals,
	}
	sc.poolMetadataCache[poolAddress] = metadata

	sc.logger.Info("cached pool metadata",
		zap.String("pool", poolName),
		zap.String("poolAddress", poolAddress.Hex()),
		zap.String("tokenAddress", tokenAddress.Hex()),
		zap.Uint8("decimals", decimals))

	return metadata, nil
}

// DiscoverPoolFromFactory queries the factory contract to find the pool address for a query type
func (sc *StakingClient) DiscoverPoolFromFactory(ctx context.Context, factoryAddress common.Address, queryType [32]byte) (common.Address, error) {
	if factoryAddress == (common.Address{}) {
		return common.Address{}, fmt.Errorf("factory address not configured")
	}

	callData := PackQueryTypePoolsCall(queryType)

	result, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &factoryAddress,
		Data: callData,
	}, nil)
	if err != nil {
		sc.logger.Debug("failed to call queryTypePools on factory",
			zap.String("factory", factoryAddress.Hex()),
			zap.String("queryType", fmt.Sprintf("%x", queryType)),
			zap.Error(err))
		return common.Address{}, fmt.Errorf("failed to call queryTypePools on factory: %w", err)
	}

	poolAddress, err := ParseSignerAddress(result) // Reuse the address parsing logic
	if err != nil {
		sc.logger.Error("failed to parse pool address from factory",
			zap.String("factory", factoryAddress.Hex()),
			zap.String("queryType", fmt.Sprintf("%x", queryType)),
			zap.Error(err))
		return common.Address{}, fmt.Errorf("failed to parse pool address from factory: %w", err)
	}

	return poolAddress, nil
}

// GetCachedPoolAddress fetches and caches pool addresses from the factory contract.
// Pool addresses for a given query type are immutable once deployed.
func (sc *StakingClient) GetCachedPoolAddress(ctx context.Context, queryTypeBits [32]byte, poolName string) (common.Address, error) {
	// Check cache first (with read lock)
	sc.cacheMutex.RLock()
	cached, exists := sc.factoryPoolCache[queryTypeBits]
	sc.cacheMutex.RUnlock()

	if exists {
		sc.logger.Debug("using cached pool address",
			zap.String("pool", poolName),
			zap.String("poolAddress", cached.Hex()))
		return cached, nil
	}

	// Not in cache, acquire write lock and fetch
	sc.cacheMutex.Lock()
	defer sc.cacheMutex.Unlock()

	// Double-check cache in case another goroutine filled it while we waited
	if cached, exists := sc.factoryPoolCache[queryTypeBits]; exists {
		return cached, nil
	}

	sc.logger.Debug("fetching pool address from factory",
		zap.String("pool", poolName),
		zap.String("queryTypeBits", fmt.Sprintf("%x", queryTypeBits)))

	// Use existing method to discover pool
	poolAddress, err := sc.DiscoverPoolFromFactory(ctx, sc.factoryAddress, queryTypeBits)
	if err != nil {
		return common.Address{}, err
	}

	// Store in cache (even zero address - means no pool exists for this query type)
	sc.factoryPoolCache[queryTypeBits] = poolAddress

	if poolAddress != (common.Address{}) {
		sc.logger.Info("cached pool address",
			zap.String("pool", poolName),
			zap.String("poolAddress", poolAddress.Hex()))
	}

	return poolAddress, nil
}

// GetConversionTableEntry queries the conversion table history at a specific index
// Deprecated: Use GetConversionTableHistory for better performance (caches full history)
func (sc *StakingClient) GetConversionTableEntry(ctx context.Context, poolAddress common.Address, index *uint256.Int, poolName string) ([32]byte, error) {
	callData := PackConversionTableHistoryCall(index)

	result, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddress,
		Data: callData,
	}, nil)
	if err != nil {
		sc.logger.Debug("failed to call conversionTableHistory",
			zap.String("pool", poolName),
			zap.String("index", index.String()),
			zap.Error(err))
		return [32]byte{}, fmt.Errorf("failed to call conversionTableHistory on pool %s: %w", poolName, err)
	}

	entry, err := ParseConversionTableEntry(result)
	if err != nil {
		sc.logger.Error("failed to parse conversion table entry",
			zap.String("pool", poolName),
			zap.String("index", index.String()),
			zap.Error(err))
		return [32]byte{}, fmt.Errorf("failed to parse conversion table entry from pool %s: %w", poolName, err)
	}

	return entry, nil
}

// GetConversionTableHistory fetches and caches the full conversion table history for a pool.
// This eliminates repeated contract calls for individual entries.
// The cache is populated lazily on first access and is thread-safe.
func (sc *StakingClient) GetConversionTableHistory(ctx context.Context, poolAddress common.Address, poolName string) ([][32]byte, error) {
	// Check cache first (with read lock)
	sc.cacheMutex.RLock()
	cached, exists := sc.conversionHistoryCache[poolAddress]
	sc.cacheMutex.RUnlock()

	if exists {
		sc.logger.Debug("using cached conversion table history",
			zap.String("pool", poolName),
			zap.String("poolAddress", poolAddress.Hex()),
			zap.Int("entries", len(cached)))
		return cached, nil
	}

	// Not in cache, acquire write lock and fetch
	sc.cacheMutex.Lock()
	defer sc.cacheMutex.Unlock()

	// Double-check cache in case another goroutine filled it while we waited
	if cached, exists := sc.conversionHistoryCache[poolAddress]; exists {
		return cached, nil
	}

	sc.logger.Info("fetching full conversion table history from contract",
		zap.String("pool", poolName),
		zap.String("poolAddress", poolAddress.Hex()))

	// Get the length of the history array
	lengthCallData := PackGetConversionTableHistoryLengthCall()
	lengthResult, err := sc.client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddress,
		Data: lengthCallData,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get conversion table history length: %w", err)
	}

	historyLength, err := ParseUint256Result(lengthResult)
	if err != nil {
		return nil, fmt.Errorf("failed to parse history length: %w", err)
	}

	length := historyLength.Uint64()
	if length == 0 {
		sc.logger.Warn("conversion table history is empty",
			zap.String("pool", poolName),
			zap.String("poolAddress", poolAddress.Hex()))
		return nil, fmt.Errorf("conversion table history is empty for pool %s", poolName)
	}

	sc.logger.Debug("fetching conversion table entries",
		zap.String("pool", poolName),
		zap.Uint64("count", length))

	// Fetch all entries
	history := make([][32]byte, length)
	for i := uint64(0); i < length; i++ {
		index := uint256.NewInt(i)
		callData := PackConversionTableHistoryCall(index)

		result, err := sc.client.CallContract(ctx, ethereum.CallMsg{
			To:   &poolAddress,
			Data: callData,
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch conversion table entry %d: %w", i, err)
		}

		entry, err := ParseConversionTableEntry(result)
		if err != nil {
			return nil, fmt.Errorf("failed to parse conversion table entry %d: %w", i, err)
		}

		history[i] = entry
	}

	// Store in cache
	sc.conversionHistoryCache[poolAddress] = history

	sc.logger.Info("cached conversion table history",
		zap.String("pool", poolName),
		zap.String("poolAddress", poolAddress.Hex()),
		zap.Int("entries", len(history)))

	return history, nil
}

// CalculateRates calculates rate limits based on stake amount and conversion tranches.
// The tranches define rate/tranche pairs locked in at stake time.
// Rates in the tranches are queries per minute (QPM).
// The decimals parameter is used to convert the stake amount from wei to token units.
func CalculateRates(stakeAmount *uint256.Int, tranches []ConversionTranche, decimals uint8) queryratelimit.Rule {
	if stakeAmount == nil || stakeAmount.Cmp(uint256.NewInt(0)) == 0 {
		return queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0}
	}

	if len(tranches) == 0 {
		return queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0}
	}

	// Convert stake amount from wei to token units using decimals
	// This allows the JSON config to use human-readable token amounts (e.g., 5000 tokens)
	// instead of wei amounts (e.g., 5000000000000000000000)
	divisor := new(uint256.Int).Exp(uint256.NewInt(10), uint256.NewInt(uint64(decimals)))
	normalizedStake := new(uint256.Int).Div(stakeAmount, divisor)
	stakeAmountUint64 := normalizedStake.Uint64()

	// Find the highest tranche that the stake qualifies for
	// Tranches are expected to be in ascending order by stake amount
	var selectedTranche *ConversionTranche

	for i := range tranches {
		if stakeAmountUint64 >= tranches[i].Tranche {
			selectedTranche = &tranches[i]
		} else {
			break // Once we exceed stake amount, stop searching
		}
	}

	// If stake doesn't meet any tranche, return zero rate
	if selectedTranche == nil {
		return queryratelimit.Rule{MaxPerSecond: 0, MaxPerMinute: 0}
	}

	// Calculate rate: (stakeAmount / tranche) * rate
	// This gives us the allotted QPM
	multiplier := stakeAmountUint64 / selectedTranche.Tranche
	qpmInt := int(multiplier * selectedTranche.Rate)

	// Convert QPM to QPS if rate is high enough (>= 60 QPM = 1 QPS)
	// This matches assumption 1: rate is always QPM, convert to QPS when appropriate
	maxPerSecond := 0
	if qpmInt >= 60 {
		maxPerSecond = qpmInt / 60
	}

	return queryratelimit.Rule{
		MaxPerSecond: maxPerSecond,
		MaxPerMinute: qpmInt,
	}
}

// FetchStakingPolicy creates a policy based on staking contract state using factory discovery
// stakerAddr is the address that holds the stake, signerAddr is the address that signed the request
// For self-staking, both addresses will be the same
func (sc *StakingClient) FetchStakingPolicy(ctx context.Context, stakerAddr, signerAddr common.Address) (*queryratelimit.Policy, error) {
	start := time.Now()

	sc.logger.Debug("fetching staking policy",
		zap.String("staker", stakerAddr.Hex()),
		zap.String("signer", signerAddr.Hex()))

	policy := &queryratelimit.Policy{
		Limits: queryratelimit.Limits{
			Types: make(map[uint8]queryratelimit.Rule),
		},
	}

	currentTime := uint64(time.Now().Unix())
	poolsChecked := 0
	poolsWithStakes := 0
	totalErrors := 0

	// Check each supported query type pool via factory
	for poolName, pool := range SupportedQueryPools {
		poolsChecked++

		// Discover pool address from factory using query type bits (cached)
		poolAddress, err := sc.GetCachedPoolAddress(ctx, pool.QueryTypeBits(), poolName)
		if err != nil {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("factory_error", poolName).Inc()
			sc.logger.Warn("failed to discover pool from factory",
				zap.String("poolName", poolName),
				zap.String("staker", stakerAddr.Hex()),
				zap.Error(err))
			continue
		}

		// Skip if no pool exists for this query type
		if poolAddress == (common.Address{}) {
			sc.logger.Debug("no pool found for query type", zap.String("queryType", poolName))
			continue
		}

		// Verify signer is authorized to act on behalf of staker
		if err := sc.VerifySignerAuthorization(ctx, poolAddress, stakerAddr, signerAddr, poolName); err != nil {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("unauthorized_signer", poolName).Inc()
			sc.logger.Warn("signer not authorized for staker",
				zap.String("poolName", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.String("signer", signerAddr.Hex()),
				zap.Error(err))
			continue
		}

		stakeInfo, err := sc.GetStakeInfo(ctx, poolAddress, stakerAddr, poolName)
		if err != nil {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("error", poolName).Inc()
			sc.logger.Warn("failed to query staking pool during policy fetch",
				zap.String("poolName", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.Error(err))
			continue
		}

		// Check stake status and record metrics
		if !stakeInfo.HasStake() {
			stakingPolicyFetches.WithLabelValues("no_stake", poolName).Inc()
			sc.logger.Debug("no stake found in pool",
				zap.String("queryType", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()))
			continue
		}

		// Check if the staker is blocklisted
		stakerBlocked, err := sc.IsBlocklisted(ctx, poolAddress, stakerAddr, poolName)
		if err != nil {
			sc.logger.Warn("failed to check staker blocklist status during policy fetch",
				zap.String("queryType", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.Error(err))
			// Continue without blocking if we can't check blocklist status
		} else if stakerBlocked {
			stakingPolicyFetches.WithLabelValues("blocklisted", poolName).Inc()
			sc.logger.Debug("staker is blocklisted in pool",
				zap.String("queryType", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()))
			continue
		}

		// Check if the signer is blocklisted (if different from staker)
		if stakerAddr != signerAddr {
			signerBlocked, err := sc.IsBlocklisted(ctx, poolAddress, signerAddr, poolName)
			if err != nil {
				sc.logger.Warn("failed to check signer blocklist status during policy fetch",
					zap.String("queryType", poolName),
					zap.String("poolAddress", poolAddress.Hex()),
					zap.String("signer", signerAddr.Hex()),
					zap.Error(err))
				// Continue without blocking if we can't check blocklist status
			} else if signerBlocked {
				stakingPolicyFetches.WithLabelValues("blocklisted", poolName).Inc()
				sc.logger.Debug("signer is blocklisted in pool",
					zap.String("queryType", poolName),
					zap.String("poolAddress", poolAddress.Hex()),
					zap.String("signer", signerAddr.Hex()))
				continue
			}
		}

		if stakeInfo.HasExpired(currentTime) {
			stakingPolicyFetches.WithLabelValues("expired", poolName).Inc()
			sc.logger.Debug("stake has expired",
				zap.String("queryType", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.Uint64("accessEnd", stakeInfo.AccessEnd),
				zap.Uint64("currentTime", currentTime))
			continue
		}

		// Valid stake found
		poolsWithStakes++
		stakingPolicyFetches.WithLabelValues("success", poolName).Inc()

		// Get cached conversion table history for this pool (lazy-loads if needed)
		conversionHistory, err := sc.GetConversionTableHistory(ctx, poolAddress, poolName)
		if err != nil {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("conversion_history_error", poolName).Inc()
			sc.logger.Warn("failed to get conversion table history during policy fetch",
				zap.String("poolName", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.Error(err))
			continue
		}

		// Validate index is within bounds
		if stakeInfo.ConversionTableIndex.Uint64() >= uint64(len(conversionHistory)) {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("invalid_index", poolName).Inc()
			sc.logger.Error("conversion table index out of bounds",
				zap.String("poolName", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.Uint64("index", stakeInfo.ConversionTableIndex.Uint64()),
				zap.Int("historyLength", len(conversionHistory)))
			continue
		}

		// Get CID from cached history using staker's index
		conversionCID := conversionHistory[stakeInfo.ConversionTableIndex.Uint64()]

		// Fetch and parse IPFS JSON (cached per CID)
		conversionTable, err := sc.ipfsClient.FetchConversionTable(ctx, conversionCID)
		if err != nil {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("ipfs_fetch_error", poolName).Inc()
			sc.logger.Warn("failed to fetch conversion table from IPFS during policy fetch",
				zap.String("poolName", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.String("staker", stakerAddr.Hex()),
				zap.Error(err))
			continue
		}

		// Get chain name from first query type (all query types in pool map to same chain)
		if len(pool.QueryTypes) == 0 {
			sc.logger.Error("pool has no query types",
				zap.String("poolName", poolName))
			continue
		}

		chainName, err := GetChainName(pool.QueryTypes[0])
		if err != nil {
			totalErrors++
			sc.logger.Error("unknown query type in pool",
				zap.String("poolName", poolName),
				zap.Uint8("queryType", uint8(pool.QueryTypes[0])),
				zap.Error(err))
			continue
		}

		// Extract chain-specific rates from IPFS data (once per pool)
		tranches, err := conversionTable.GetTranchesByChain(chainName)
		if err != nil {
			totalErrors++
			stakingPolicyFetches.WithLabelValues("chain_parse_error", poolName).Inc()
			sc.logger.Warn("failed to get tranches for chain during policy fetch",
				zap.String("poolName", poolName),
				zap.String("chainName", chainName),
				zap.String("staker", stakerAddr.Hex()),
				zap.Error(err))
			continue
		}

		// Fetch pool metadata for proper stake amount conversion (cached)
		poolMetadata, err := sc.GetPoolMetadata(ctx, poolAddress, poolName)
		var decimals uint8 = 18 // Default to 18 decimals (standard ERC20)
		if err != nil {
			sc.logger.Warn("failed to get pool metadata, defaulting to 18 decimals",
				zap.String("poolName", poolName),
				zap.String("poolAddress", poolAddress.Hex()),
				zap.Error(err))
		} else {
			decimals = poolMetadata.TokenDecimals
		}

		// Calculate rate limits using tranches
		rates := CalculateRates(stakeInfo.Amount, tranches, decimals)

		sc.logger.Info("rate calculation details",
			zap.String("pool", poolName),
			zap.String("stakeAmount", stakeInfo.Amount.String()),
			zap.Uint8("decimals", decimals),
			zap.Int("trancheCount", len(tranches)),
			zap.Int("maxPerSecond", rates.MaxPerSecond),
			zap.Int("maxPerMinute", rates.MaxPerMinute))

		// Determine tier for metrics
		tier := "none"
		if rates.MaxPerSecond > 0 {
			tier = "qps"
		} else if rates.MaxPerMinute > 0 {
			tier = "qpm"
		}

		// Skip if no rates calculated
		if rates.MaxPerSecond == 0 && rates.MaxPerMinute == 0 {
			stakingPolicyDecisions.WithLabelValues("denied", tier, "all").Inc()
			continue
		}

		sc.logger.Debug("calculated rates for pool",
			zap.String("queryType", poolName),
			zap.String("chainName", chainName),
			zap.String("poolAddress", poolAddress.Hex()),
			zap.String("signer", signerAddr.Hex()),
			zap.String("tier", tier),
			zap.Int("maxPerSecond", rates.MaxPerSecond),
			zap.Int("maxPerMinute", rates.MaxPerMinute),
			zap.String("stakeAmount", stakeInfo.Amount.String()))

		// Apply rates to all query types in this pool
		for _, queryType := range pool.QueryTypes {
			queryTypeStr := fmt.Sprintf("%d", queryType)
			qt := uint8(queryType)

			// Record policy decision
			stakingPolicyDecisions.WithLabelValues("allowed", tier, queryTypeStr).Inc()

			// If multiple pools grant access to the same query type, take the maximum
			if existingRule, exists := policy.Limits.Types[qt]; exists {
				updated := false
				if rates.MaxPerSecond > existingRule.MaxPerSecond {
					existingRule.MaxPerSecond = rates.MaxPerSecond
					updated = true
				}
				if rates.MaxPerMinute > existingRule.MaxPerMinute {
					existingRule.MaxPerMinute = rates.MaxPerMinute
					updated = true
				}
				policy.Limits.Types[qt] = existingRule

				if updated {
					sc.logger.Debug("updated existing policy with higher limits",
						zap.String("queryType", poolName),
						zap.Uint8("queryType", qt),
						zap.Int("newMaxPerSecond", existingRule.MaxPerSecond),
						zap.Int("newMaxPerMinute", existingRule.MaxPerMinute))
				}
			} else {
				policy.Limits.Types[qt] = rates
				sc.logger.Debug("added new policy",
					zap.String("queryType", poolName),
					zap.Uint8("queryType", qt),
					zap.Int("maxPerSecond", rates.MaxPerSecond),
					zap.Int("maxPerMinute", rates.MaxPerMinute))
			}
		}
	}

	// Final logging and metrics
	totalQueryTypes := len(policy.Limits.Types)
	policyFetchDuration := time.Since(start)

	sc.logger.Info("completed staking policy fetch",
		zap.String("staker", stakerAddr.Hex()),
		zap.String("signer", signerAddr.Hex()),
		zap.Int("queryTypesChecked", poolsChecked),
		zap.Int("poolsWithStakes", poolsWithStakes),
		zap.Int("totalErrors", totalErrors),
		zap.Int("allowedQueryTypes", totalQueryTypes),
		zap.Duration("fetchDuration", policyFetchDuration))

	// Record overall policy fetch result
	if totalQueryTypes > 0 {
		stakingPolicyFetches.WithLabelValues("policy_created", "all").Inc()
	} else {
		stakingPolicyFetches.WithLabelValues("no_access", "all").Inc()
	}

	return policy, nil
}
