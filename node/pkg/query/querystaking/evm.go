package querystaking

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

type StakeAndSigner struct {
	StakeInfo *StakeInfo     `abi:"StakeInfo"`
	Signer    common.Address `abi:"Signer"`
}

type StakeInfo struct {
	Amount               *uint256.Int `abi:"uint256"`
	ConversionTableIndex *uint256.Int `abi:"uint256"`
	LockupEnd            uint64       `abi:"uint64"`
	AccessEnd            uint64       `abi:"uint64"`
	LastClaimed          uint64       `abi:"uint64"`
	Capacity             *uint256.Int `abi:"uint256"`
}

var ErrInvalidLength = errors.New("invalid length")

// ParseStakeInfo parses raw contract call data into a StakeInfo struct.
func ParseStakeInfo(data []byte) (*StakeInfo, error) {
	expectedLength := 32 * 6

	if len(data) != expectedLength {
		return nil, fmt.Errorf("invalid length: got %d want %d", len(data), expectedLength)
	}

	stakeInfo := &StakeInfo{}

	stakeInfo.Amount = uint256.NewInt(0).SetBytes(data[0:32])
	stakeInfo.ConversionTableIndex = uint256.NewInt(0).SetBytes(data[32:64])

	tmp := uint256.NewInt(0)
	stakeInfo.LockupEnd = tmp.SetBytes(data[64:96]).Uint64()
	stakeInfo.AccessEnd = tmp.SetBytes(data[96:128]).Uint64()
	stakeInfo.LastClaimed = tmp.SetBytes(data[128:160]).Uint64()
	stakeInfo.Capacity = uint256.NewInt(0).SetBytes(data[160:192])

	return stakeInfo, nil
}

// PackStakesCall creates call data for the stakes(address) function.
func PackStakesCall(staker common.Address) []byte {
	selector := crypto.Keccak256([]byte("stakes(address)"))[:4]
	paddedAddress := common.LeftPadBytes(staker.Bytes(), 32)

	result := make([]byte, 4+32)
	copy(result[0:4], selector)
	copy(result[4:36], paddedAddress)

	return result
}

// PackStakerSignersCall creates call data for the stakerSigners(address) function.
func PackStakerSignersCall(staker common.Address) []byte {
	selector := crypto.Keccak256([]byte("stakerSigners(address)"))[:4]
	paddedAddress := common.LeftPadBytes(staker.Bytes(), 32)

	result := make([]byte, 4+32)
	copy(result[0:4], selector)
	copy(result[4:36], paddedAddress)

	return result
}

// ParseSignerAddress parses the result of stakerSigners(address) call.
func ParseSignerAddress(data []byte) (common.Address, error) {
	if len(data) != 32 {
		return common.Address{}, fmt.Errorf("invalid signer data length: got %d want 32", len(data))
	}

	return common.BytesToAddress(data[12:32]), nil
}

// PackIsBlocklistedCall creates call data for the isBlocklisted(address) function.
func PackIsBlocklistedCall(user common.Address) []byte {
	selector := crypto.Keccak256([]byte("isBlocklisted(address)"))[:4]
	paddedAddress := common.LeftPadBytes(user.Bytes(), 32)

	result := make([]byte, 4+32)
	copy(result[0:4], selector)
	copy(result[4:36], paddedAddress)

	return result
}

// ParseBoolResult parses the result of a bool-returning contract call.
func ParseBoolResult(data []byte) (bool, error) {
	if len(data) != 32 {
		return false, fmt.Errorf("invalid bool data length: got %d want 32", len(data))
	}

	// The last byte of the 32-byte result contains the boolean value
	return data[31] != 0, nil
}

// PackQueryTypePoolsCall creates call data for the queryTypePools(bytes32) function.
func PackQueryTypePoolsCall(queryType [32]byte) []byte {
	selector := crypto.Keccak256([]byte("queryTypePools(bytes32)"))[:4]

	result := make([]byte, 4+32)
	copy(result[0:4], selector)
	copy(result[4:36], queryType[:])

	return result
}

// PackConversionTableHistoryCall creates call data for the conversionTableHistory(uint256) function.
func PackConversionTableHistoryCall(index *uint256.Int) []byte {
	selector := crypto.Keccak256([]byte("conversionTableHistory(uint256)"))[:4]
	paddedIndex := common.LeftPadBytes(index.Bytes(), 32)

	result := make([]byte, 4+32)
	copy(result[0:4], selector)
	copy(result[4:36], paddedIndex)

	return result
}

// ParseConversionTableEntry parses the result of conversionTableHistory(uint256) call.
// The conversion table entry is a bytes32 value.
func ParseConversionTableEntry(data []byte) ([32]byte, error) {
	if len(data) != 32 {
		return [32]byte{}, fmt.Errorf("invalid conversion table entry length: got %d want 32", len(data))
	}

	var entry [32]byte
	copy(entry[:], data)
	return entry, nil
}

// PackGetConversionTableHistoryLengthCall creates call data for the getConversionTableHistoryLength() function.
func PackGetConversionTableHistoryLengthCall() []byte {
	selector := crypto.Keccak256([]byte("getConversionTableHistoryLength()"))[:4]
	return selector
}

// ParseUint256Result parses a uint256 result from a contract call.
func ParseUint256Result(data []byte) (*uint256.Int, error) {
	if len(data) != 32 {
		return nil, fmt.Errorf("invalid uint256 data length: got %d want 32", len(data))
	}
	return uint256.NewInt(0).SetBytes(data), nil
}

// PackStakingTokenCall creates call data for the STAKING_TOKEN() function.
func PackStakingTokenCall() []byte {
	selector := crypto.Keccak256([]byte("STAKING_TOKEN()"))[:4]
	return selector
}

// PackDecimalsCall creates call data for the decimals() function on an ERC20 token.
func PackDecimalsCall() []byte {
	selector := crypto.Keccak256([]byte("decimals()"))[:4]
	return selector
}

// ParseAddressResult parses an address result from a contract call.
func ParseAddressResult(data []byte) (common.Address, error) {
	if len(data) != 32 {
		return common.Address{}, fmt.Errorf("invalid address data length: got %d want 32", len(data))
	}
	return common.BytesToAddress(data[12:32]), nil
}

// ParseUint8Result parses a uint8 result from a contract call.
func ParseUint8Result(data []byte) (uint8, error) {
	if len(data) != 32 {
		return 0, fmt.Errorf("invalid uint8 data length: got %d want 32", len(data))
	}
	return uint8(data[31]), nil
}

// ConversionTranche represents a single tranche in the conversion table
type ConversionTranche struct {
	Rate    uint64 // Queries per minute for this tranche
	Tranche uint64 // Minimum stake amount required for this tranche
}

// ParseConversionTranches parses a bitpacked conversion table entry into tranches.
// Format: "rate:100,tranche:1000,rate:6000,tranche:20000"
// Returns tranches sorted by tranche amount (ascending).
// Deprecated: This function is for the old string-based format. Use ConversionTable.GetTranchesByChain for IPFS-based conversion tables.
func ParseConversionTranches(entry [32]byte) ([]ConversionTranche, error) {
	// Convert bytes32 to string, removing null bytes
	str := string(entry[:])
	// Trim null bytes
	str = strings.TrimRight(str, "\x00")

	if str == "" {
		return nil, fmt.Errorf("empty conversion table entry")
	}

	// Split by comma
	parts := strings.Split(str, ",")
	if len(parts)%2 != 0 {
		return nil, fmt.Errorf("invalid conversion table format: odd number of parts")
	}

	// Process pairs: rate,tranche,rate,tranche,...
	var tranches []ConversionTranche
	for i := 0; i < len(parts); i += 2 {
		ratePart := parts[i]
		tranchePart := parts[i+1]

		// Parse rate
		rateKV := strings.SplitN(ratePart, ":", 2)
		if len(rateKV) != 2 || strings.TrimSpace(rateKV[0]) != "rate" {
			return nil, fmt.Errorf("expected 'rate:value' at position %d, got '%s'", i, ratePart)
		}
		rate, err := strconv.ParseUint(strings.TrimSpace(rateKV[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse rate value '%s': %w", rateKV[1], err)
		}

		// Parse tranche
		trancheKV := strings.SplitN(tranchePart, ":", 2)
		if len(trancheKV) != 2 || strings.TrimSpace(trancheKV[0]) != "tranche" {
			return nil, fmt.Errorf("expected 'tranche:value' at position %d, got '%s'", i+1, tranchePart)
		}
		tranche, err := strconv.ParseUint(strings.TrimSpace(trancheKV[1]), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse tranche value '%s': %w", trancheKV[1], err)
		}

		tranches = append(tranches, ConversionTranche{
			Rate:    rate,
			Tranche: tranche,
		})
	}

	if len(tranches) == 0 {
		return nil, fmt.Errorf("no tranches found in conversion table entry")
	}

	return tranches, nil
}

// HasStake returns true if the stake amount is greater than zero.
func (si *StakeInfo) HasStake() bool {
	return si.Amount != nil && si.Amount.Cmp(uint256.NewInt(0)) > 0
}

// IsInLockup returns true if the stake is still in the lockup period.
func (si *StakeInfo) IsInLockup(currentTimestamp uint64) bool {
	return currentTimestamp < si.LockupEnd
}

// IsInAccessPeriod returns true if the stake is in the access period.
func (si *StakeInfo) IsInAccessPeriod(currentTimestamp uint64) bool {
	return currentTimestamp >= si.LockupEnd && currentTimestamp < si.AccessEnd
}

// HasExpired returns true if the stake has passed the access period.
func (si *StakeInfo) HasExpired(currentTimestamp uint64) bool {
	return currentTimestamp >= si.AccessEnd
}

// ConversionTable represents the IPFS JSON structure for chain-specific conversion rates
type ConversionTable struct {
	EVM    map[string]string `json:"EVM"`
	Solana map[string]string `json:"Solana"`
}

// GetTranchesByChain extracts and parses tranches for a specific chain
// Returns sorted tranches (ascending by tranche amount)
func (ct *ConversionTable) GetTranchesByChain(chainName string) ([]ConversionTranche, error) {
	var chainRates map[string]string

	// Select the appropriate chain data
	switch chainName {
	case "EVM":
		chainRates = ct.EVM
	case "Solana":
		chainRates = ct.Solana
	default:
		return nil, fmt.Errorf("unknown chain: %s", chainName)
	}

	if chainRates == nil {
		return nil, fmt.Errorf("no rates found for chain: %s", chainName)
	}

	// Parse the map into tranches
	tranches := make([]ConversionTranche, 0, len(chainRates))
	for trancheStr, rateStr := range chainRates {
		// Parse tranche amount
		tranche, err := strconv.ParseUint(trancheStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid tranche amount '%s': %w", trancheStr, err)
		}

		// Parse rate string (e.g., "1 QPS" or "5 QPM")
		rate, err := parseRateString(rateStr)
		if err != nil {
			return nil, fmt.Errorf("invalid rate string '%s': %w", rateStr, err)
		}

		tranches = append(tranches, ConversionTranche{
			Rate:    rate,
			Tranche: tranche,
		})
	}

	if len(tranches) == 0 {
		return nil, fmt.Errorf("no valid tranches found for chain: %s", chainName)
	}

	// Sort by tranche amount (ascending)
	sort.Slice(tranches, func(i, j int) bool {
		return tranches[i].Tranche < tranches[j].Tranche
	})

	return tranches, nil
}

// parseRateString parses rate strings like "1 QPS" or "5 QPM" into queries per minute
// QPS (queries per second) is converted to QPM by multiplying by 60
// QPM (queries per minute) is used directly
func parseRateString(rateStr string) (uint64, error) {
	parts := strings.Fields(rateStr)
	if len(parts) != 2 {
		return 0, fmt.Errorf("rate string must have format '<number> <unit>', got: %s", rateStr)
	}

	// Parse the numeric value
	value, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid rate value '%s': %w", parts[0], err)
	}

	// Parse the unit and convert to QPM
	unit := strings.ToUpper(parts[1])
	switch unit {
	case "QPS":
		// Convert QPS to QPM
		return value * 60, nil
	case "QPM":
		// Already in QPM
		return value, nil
	default:
		return 0, fmt.Errorf("unknown rate unit '%s', expected QPS or QPM", parts[1])
	}
}
