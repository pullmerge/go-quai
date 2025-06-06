// Copyright 2014 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package vm

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"math/big"

	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/common/math"
	"github.com/dominant-strategies/go-quai/core/rawdb"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/crypto"
	"github.com/dominant-strategies/go-quai/crypto/blake2b"
	"github.com/dominant-strategies/go-quai/crypto/bn256"
	"github.com/dominant-strategies/go-quai/ethdb"
	"github.com/dominant-strategies/go-quai/log"
	"github.com/dominant-strategies/go-quai/params"
	"github.com/holiman/uint256"
	"github.com/sirupsen/logrus"

	//lint:ignore SA1019 Needed for precompile
	"golang.org/x/crypto/ripemd160"
)

// PrecompiledContract is the basic interface for native Go contracts. The implementation
// requires a deterministic gas count based on the input size of the Run method of the
// contract.
type PrecompiledContract interface {
	RequiredGas(input []byte) uint64  // RequiredPrice calculates the contract gas use
	Run(input []byte) ([]byte, error) // Run runs the precompiled contract
}

var (
	PrecompiledContracts    map[common.AddressBytes]PrecompiledContract = make(map[common.AddressBytes]PrecompiledContract)
	PrecompiledAddresses    map[string][]common.Address                 = make(map[string][]common.Address)
	LockupContractAddresses map[[2]byte]common.Address                  = make(map[[2]byte]common.Address) // LockupContractAddress is not of type PrecompiledContract
)

func InitializePrecompiles(nodeLocation common.Location) {
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000001", nodeLocation.BytePrefix()))] = &ecrecover{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000002", nodeLocation.BytePrefix()))] = &sha256hash{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000003", nodeLocation.BytePrefix()))] = &ripemd160hash{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000004", nodeLocation.BytePrefix()))] = &dataCopy{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000005", nodeLocation.BytePrefix()))] = &bigModExp{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000006", nodeLocation.BytePrefix()))] = &bn256Add{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000007", nodeLocation.BytePrefix()))] = &bn256ScalarMul{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000008", nodeLocation.BytePrefix()))] = &bn256Pairing{}
	PrecompiledContracts[common.HexToAddressBytes(fmt.Sprintf("0x%02x00000000000000000000000000000000000009", nodeLocation.BytePrefix()))] = &blake2F{}
	LockupContractAddresses[[2]byte{nodeLocation[0], nodeLocation[1]}] = common.HexToAddress(fmt.Sprintf("0x%02x0000000000000000000000000000000000000A", nodeLocation.BytePrefix()), nodeLocation)

	for address, _ := range PrecompiledContracts {
		if address.Location().Equal(nodeLocation) {
			PrecompiledAddresses[nodeLocation.Name()] = append(PrecompiledAddresses[nodeLocation.Name()], common.BytesToAddress(address[:], nodeLocation))
			PrecompiledAddresses[nodeLocation.Name()] = append(PrecompiledAddresses[nodeLocation.Name()], common.HexToAddress(fmt.Sprintf("0x00000000000000000000000000000000000000%02x", address[19]), nodeLocation))
		}
	}
}

// ActivePrecompiles returns the precompiles enabled with the current configuration, except the Lockup Contract.
func ActivePrecompiles(rules params.Rules, nodeLocation common.Location) []common.Address {
	return PrecompiledAddresses[nodeLocation.Name()]
}

// RunPrecompiledContract runs and evaluates the output of a precompiled contract.
// It returns
// - the returned bytes,
// - the _remaining_ gas,
// - any error that occurred
func RunPrecompiledContract(p PrecompiledContract, input []byte, suppliedGas uint64) (ret []byte, remainingGas uint64, err error) {
	gasCost := p.RequiredGas(input)
	if suppliedGas < gasCost {
		return nil, 0, ErrOutOfGas
	}
	suppliedGas -= gasCost
	output, err := p.Run(input)
	return output, suppliedGas, err
}

// ECRECOVER implemented as a native contract.
type ecrecover struct{}

func (c *ecrecover) RequiredGas(input []byte) uint64 {
	return params.EcrecoverGas
}

func (c *ecrecover) Run(input []byte) ([]byte, error) {
	const ecRecoverInputLength = 128

	input = common.RightPadBytes(input, ecRecoverInputLength)
	// "input" is (hash, v, r, s), each 32 bytes
	// but for ecrecover we want (r, s, v)

	r := new(big.Int).SetBytes(input[64:96])
	s := new(big.Int).SetBytes(input[96:128])
	v := input[63] - 27

	// tighter sig s values input only apply to tx sigs
	if !allZero(input[32:63]) || !crypto.ValidateSignatureValues(v, r, s) {
		return nil, nil
	}
	// We must make sure not to modify the 'input', so placing the 'v' along with
	// the signature needs to be done on a new allocation
	sig := make([]byte, 65)
	copy(sig, input[64:128])
	sig[64] = v
	// v needs to be at the end for libsecp256k1
	pubKey, err := crypto.Ecrecover(input[:32], sig)
	// make sure the public key is a valid one
	if err != nil {
		return nil, nil
	}

	// the first byte of pubkey is bitcoin heritage
	return common.LeftPadBytes(crypto.Keccak256(pubKey[1:])[12:], 32), nil
}

// SHA256 implemented as a native contract.
type sha256hash struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
//
// This method does not require any overflow checking as the input size gas costs
// required for anything significant is so high it's impossible to pay for.
func (c *sha256hash) RequiredGas(input []byte) uint64 {
	return uint64(len(input)+31)/32*params.Sha256PerWordGas + params.Sha256BaseGas
}
func (c *sha256hash) Run(input []byte) ([]byte, error) {
	h := sha256.Sum256(input)
	return h[:], nil
}

// RIPEMD160 implemented as a native contract.
type ripemd160hash struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
//
// This method does not require any overflow checking as the input size gas costs
// required for anything significant is so high it's impossible to pay for.
func (c *ripemd160hash) RequiredGas(input []byte) uint64 {
	return uint64(len(input)+31)/32*params.Ripemd160PerWordGas + params.Ripemd160BaseGas
}
func (c *ripemd160hash) Run(input []byte) ([]byte, error) {
	ripemd := ripemd160.New()
	ripemd.Write(input)
	return common.LeftPadBytes(ripemd.Sum(nil), 32), nil
}

// data copy implemented as a native contract.
type dataCopy struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
//
// This method does not require any overflow checking as the input size gas costs
// required for anything significant is so high it's impossible to pay for.
func (c *dataCopy) RequiredGas(input []byte) uint64 {
	return uint64(len(input)+31)/32*params.IdentityPerWordGas + params.IdentityBaseGas
}
func (c *dataCopy) Run(in []byte) ([]byte, error) {
	return in, nil
}

// bigModExp implements a native big integer exponential modular operation.
type bigModExp struct {
}

// modexpMultComplexity implements bigModexp multComplexity formula
//
// def mult_complexity(x):
//
//	if x <= 64: return x ** 2
//	elif x <= 1024: return x ** 2 // 4 + 96 * x - 3072
//	else: return x ** 2 // 16 + 480 * x - 199680
//
// where is x is max(length_of_MODULUS, length_of_BASE)
func modexpMultComplexity(x *big.Int) *big.Int {
	switch {
	case x.Cmp(common.Big64) <= 0:
		x.Mul(x, x) // x ** 2
	case x.Cmp(common.Big1024) <= 0:
		// (x ** 2 // 4 ) + ( 96 * x - 3072)
		x = new(big.Int).Add(
			new(big.Int).Div(new(big.Int).Mul(x, x), common.Big4),
			new(big.Int).Sub(new(big.Int).Mul(common.Big96, x), common.Big3072),
		)
	default:
		// (x ** 2 // 16) + (480 * x - 199680)
		x = new(big.Int).Add(
			new(big.Int).Div(new(big.Int).Mul(x, x), common.Big16),
			new(big.Int).Sub(new(big.Int).Mul(common.Big480, x), common.Big199680),
		)
	}
	return x
}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bigModExp) RequiredGas(input []byte) uint64 {
	var (
		baseLen = new(big.Int).SetBytes(getData(input, 0, 32))
		expLen  = new(big.Int).SetBytes(getData(input, 32, 32))
		modLen  = new(big.Int).SetBytes(getData(input, 64, 32))
	)
	if len(input) > 96 {
		input = input[96:]
	} else {
		input = input[:0]
	}
	// Retrieve the head 32 bytes of exp for the adjusted exponent length
	var expHead *big.Int
	if big.NewInt(int64(len(input))).Cmp(baseLen) <= 0 {
		expHead = new(big.Int)
	} else {
		if expLen.Cmp(common.Big32) > 0 {
			expHead = new(big.Int).SetBytes(getData(input, baseLen.Uint64(), 32))
		} else {
			expHead = new(big.Int).SetBytes(getData(input, baseLen.Uint64(), expLen.Uint64()))
		}
	}
	// Calculate the adjusted exponent length
	var msb int
	if bitlen := expHead.BitLen(); bitlen > 0 {
		msb = bitlen - 1
	}
	adjExpLen := new(big.Int)
	if expLen.Cmp(common.Big32) > 0 {
		adjExpLen.Sub(expLen, common.Big32)
		adjExpLen.Mul(common.Big8, adjExpLen)
	}
	adjExpLen.Add(adjExpLen, big.NewInt(int64(msb)))
	// Calculate the gas cost of the operation
	gas := new(big.Int).Set(math.BigMax(modLen, baseLen))
	gas = gas.Add(gas, common.Big7)
	gas = gas.Div(gas, common.Big8)
	gas.Mul(gas, gas)

	gas.Mul(gas, math.BigMax(adjExpLen, common.Big1))

	gas.Div(gas, common.Big3)
	if gas.BitLen() > 64 {
		return math.MaxUint64
	}

	if gas.Uint64() < 200 {
		return 200
	}
	return gas.Uint64()
}

func (c *bigModExp) Run(input []byte) ([]byte, error) {
	var (
		baseLen = new(big.Int).SetBytes(getData(input, 0, 32)).Uint64()
		expLen  = new(big.Int).SetBytes(getData(input, 32, 32)).Uint64()
		modLen  = new(big.Int).SetBytes(getData(input, 64, 32)).Uint64()
	)
	if len(input) > 96 {
		input = input[96:]
	} else {
		input = input[:0]
	}
	// Handle a special case when both the base and mod length is zero
	if baseLen == 0 && modLen == 0 {
		return []byte{}, nil
	}
	// Retrieve the operands and execute the exponentiation
	var (
		base = new(big.Int).SetBytes(getData(input, 0, baseLen))
		exp  = new(big.Int).SetBytes(getData(input, baseLen, expLen))
		mod  = new(big.Int).SetBytes(getData(input, baseLen+expLen, modLen))
	)
	if mod.BitLen() == 0 {
		// Modulo 0 is undefined, return zero
		return common.LeftPadBytes([]byte{}, int(modLen)), nil
	}
	return common.LeftPadBytes(base.Exp(base, exp, mod).Bytes(), int(modLen)), nil
}

// newCurvePoint unmarshals a binary blob into a bn256 elliptic curve point,
// returning it, or an error if the point is invalid.
func newCurvePoint(blob []byte) (*bn256.G1, error) {
	p := new(bn256.G1)
	if _, err := p.Unmarshal(blob); err != nil {
		return nil, err
	}
	return p, nil
}

// newTwistPoint unmarshals a binary blob into a bn256 elliptic curve point,
// returning it, or an error if the point is invalid.
func newTwistPoint(blob []byte) (*bn256.G2, error) {
	p := new(bn256.G2)
	if _, err := p.Unmarshal(blob); err != nil {
		return nil, err
	}
	return p, nil
}

// runBn256Add implements the Bn256Add precompile
func runBn256Add(input []byte) ([]byte, error) {
	x, err := newCurvePoint(getData(input, 0, 64))
	if err != nil {
		return nil, err
	}
	y, err := newCurvePoint(getData(input, 64, 64))
	if err != nil {
		return nil, err
	}
	res := new(bn256.G1)
	res.Add(x, y)
	return res.Marshal(), nil
}

// bn256Add implements a native elliptic curve point addition conforming to consensus rules.
type bn256Add struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256Add) RequiredGas(input []byte) uint64 {
	return params.Bn256AddGas
}

func (c *bn256Add) Run(input []byte) ([]byte, error) {
	return runBn256Add(input)
}

// runBn256ScalarMul implements the Bn256ScalarMul precompile
func runBn256ScalarMul(input []byte) ([]byte, error) {
	p, err := newCurvePoint(getData(input, 0, 64))
	if err != nil {
		return nil, err
	}
	res := new(bn256.G1)
	res.ScalarMult(p, new(big.Int).SetBytes(getData(input, 64, 32)))
	return res.Marshal(), nil
}

// bn256ScalarMul implements a native elliptic curve scalar
// multiplication conforming to  consensus rules.
type bn256ScalarMul struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256ScalarMul) RequiredGas(input []byte) uint64 {
	return params.Bn256ScalarMulGas
}

func (c *bn256ScalarMul) Run(input []byte) ([]byte, error) {
	return runBn256ScalarMul(input)
}

var (
	// true32Byte is returned if the bn256 pairing check succeeds.
	true32Byte = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}

	// false32Byte is returned if the bn256 pairing check fails.
	false32Byte = make([]byte, 32)

	// errBadPairingInput is returned if the bn256 pairing input is invalid.
	errBadPairingInput = errors.New("bad elliptic curve pairing size")
)

// runBn256Pairing implements the Bn256Pairing precompile
func runBn256Pairing(input []byte) ([]byte, error) {
	// Handle some corner cases cheaply
	if len(input)%192 > 0 {
		return nil, errBadPairingInput
	}
	// Convert the input into a set of coordinates
	var (
		cs []*bn256.G1
		ts []*bn256.G2
	)
	for i := 0; i < len(input); i += 192 {
		c, err := newCurvePoint(input[i : i+64])
		if err != nil {
			return nil, err
		}
		t, err := newTwistPoint(input[i+64 : i+192])
		if err != nil {
			return nil, err
		}
		cs = append(cs, c)
		ts = append(ts, t)
	}
	// Execute the pairing checks and return the results
	if bn256.PairingCheck(cs, ts) {
		return true32Byte, nil
	}
	return false32Byte, nil
}

// bn256Pairing implements a pairing pre-compile for the bn256 curve
// conforming to  consensus rules.
type bn256Pairing struct{}

// RequiredGas returns the gas required to execute the pre-compiled contract.
func (c *bn256Pairing) RequiredGas(input []byte) uint64 {
	return params.Bn256PairingBaseGas + uint64(len(input)/192)*params.Bn256PairingPerPointGas
}

func (c *bn256Pairing) Run(input []byte) ([]byte, error) {
	return runBn256Pairing(input)
}

type blake2F struct{}

func (c *blake2F) RequiredGas(input []byte) uint64 {
	// If the input is malformed, we can't calculate the gas, return 0 and let the
	// actual call choke and fault.
	if len(input) != blake2FInputLength {
		return 0
	}
	return uint64(binary.BigEndian.Uint32(input[0:4]))
}

const (
	blake2FInputLength        = 213
	blake2FFinalBlockBytes    = byte(1)
	blake2FNonFinalBlockBytes = byte(0)
)

var (
	errBlake2FInvalidInputLength = errors.New("invalid input length")
	errBlake2FInvalidFinalFlag   = errors.New("invalid final flag")
)

func (c *blake2F) Run(input []byte) ([]byte, error) {
	// Make sure the input is valid (correct length and final flag)
	if len(input) != blake2FInputLength {
		return nil, errBlake2FInvalidInputLength
	}
	if input[212] != blake2FNonFinalBlockBytes && input[212] != blake2FFinalBlockBytes {
		return nil, errBlake2FInvalidFinalFlag
	}
	// Parse the input into the Blake2b call parameters
	var (
		rounds = binary.BigEndian.Uint32(input[0:4])
		final  = (input[212] == blake2FFinalBlockBytes)

		h [8]uint64
		m [16]uint64
		t [2]uint64
	)
	for i := 0; i < 8; i++ {
		offset := 4 + i*8
		h[i] = binary.LittleEndian.Uint64(input[offset : offset+8])
	}
	for i := 0; i < 16; i++ {
		offset := 68 + i*8
		m[i] = binary.LittleEndian.Uint64(input[offset : offset+8])
	}
	t[0] = binary.LittleEndian.Uint64(input[196:204])
	t[1] = binary.LittleEndian.Uint64(input[204:212])

	// Execute the compression function, extract and return the result
	blake2b.F(&h, m, t, final, rounds)

	output := make([]byte, 64)
	for i := 0; i < 8; i++ {
		offset := i * 8
		binary.LittleEndian.PutUint64(output[offset:offset+8], h[i])
	}
	return output, nil
}

func intToByteArray20(n uint8) [20]byte {
	var byteArray [20]byte
	byteArray[19] = byte(n) // Use the last byte for the integer
	return byteArray
}

func RequiredGas(input []byte) uint64 {
	return 0
}

// RunLockupContract is an API that ties together the Lockup Contract with the EVM.
// The requested function is determined by input length. Contracts should ensure that their input is tightly packed
// with abi.encodePacked.
func RunLockupContract(evm *EVM, ownerContract common.Address, gas *uint64, input []byte) ([]byte, error) {
	if evm.interpreter.readOnly {
		return nil, ErrWriteProtection
	}
	switch len(input) {
	case 60:
		if err := UnwrapQi(evm, ownerContract, gas, input); err != nil {
			return nil, err
		} else {
			return []byte{1}, nil
		}
	case 20:
		ret, err := ClaimQiDeposit(evm, ownerContract, gas, input)
		if err != nil {
			return nil, err
		} else {
			return ret, nil
		}
	case 53:
		if err := ClaimCoinbaseLockup(evm, ownerContract, gas, input); err != nil {
			return nil, err
		} else {
			return []byte{1}, nil
		}
	case 25:
		ret, err := GetLockupData(evm, ownerContract, input)
		if err != nil {
			return nil, err
		} else {
			return ret, nil
		}
	case 21:
		ret, err := GetLatestLockupData(evm, ownerContract, input)
		if err != nil {
			return nil, err
		} else {
			return ret, nil
		}
	default:
		return nil, ErrExecutionReverted
	}
}

func GetLockupData(evm *EVM, ownerContract common.Address, input []byte) ([]byte, error) {
	if len(input) != 25 {
		return nil, errors.New("input length is not 25 bytes")
	}
	// Extract beneficiaryMiner
	beneficiaryMiner := common.BytesToAddress(input[:20], evm.chainConfig.Location)
	// Extract lockupByte
	lockupByte := input[20]

	// Extract epoch
	epoch := binary.BigEndian.Uint32(input[21:25])
	_, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	_, err = beneficiaryMiner.InternalAddress()
	if err != nil {
		return nil, err
	}
	balance, trancheUnlockHeight, elements, delegate := rawdb.ReadCoinbaseLockup(evm.StateDB.UnderlyingDatabase(), evm.Batch, ownerContract, beneficiaryMiner, lockupByte, epoch)

	return ABIEncodeLockupData(trancheUnlockHeight, balance, elements, delegate)
}

func GetLatestLockupData(evm *EVM, ownerContract common.Address, input []byte) ([]byte, error) {
	if len(input) != 21 {
		return nil, errors.New("input length is not 21 bytes")
	}
	// Extract beneficiaryMiner
	beneficiaryMiner := common.BytesToAddress(input[:20], evm.chainConfig.Location)
	// Extract lockupByte
	lockupByte := input[20]
	currentEpoch := uint32((evm.Context.BlockNumber.Uint64() / params.CoinbaseEpochBlocks) + 1) // zero epoch is an invalid state
	balance, trancheUnlockHeight, elements, delegate := rawdb.ReadCoinbaseLockup(evm.StateDB.UnderlyingDatabase(), evm.Batch, ownerContract, beneficiaryMiner, lockupByte, currentEpoch)
	return ABIEncodeLockupData(trancheUnlockHeight, balance, elements, delegate)
}

func ABIEncodeLockupData(trancheUnlockHeight uint32, balance *big.Int, elements uint16, delegate common.Address) ([]byte, error) {
	// Create a buffer for the result
	encoded := make([]byte, 0, 128) // 32 bytes for each value

	// Encode trancheUnlockHeight (uint32, right-aligned to 32 bytes)
	trancheBytes := make([]byte, 32)
	binary.BigEndian.PutUint32(trancheBytes[28:], trancheUnlockHeight) // Right-align
	encoded = append(encoded, trancheBytes...)
	// Encode balance (32 bytes)
	balanceBytes, overflow := uint256.FromBig(balance)
	if overflow {
		return nil, fmt.Errorf("balance is too large to encode: %v", balance)
	}
	temp := balanceBytes.Bytes32()
	encoded = append(encoded, temp[:]...)

	// Encode elements (uint16, right-aligned to 32 bytes)
	elementsBytes := make([]byte, 32)
	binary.BigEndian.PutUint16(elementsBytes[30:], elements) // Right-align
	encoded = append(encoded, elementsBytes...)

	// Encode delegate (20 bytes, right-aligned to 32 bytes)
	delegateBytes := delegate.Bytes()
	encoded = append(encoded, common.LeftPadBytes(delegateBytes, 32)...)

	return encoded, nil
}

func GetAllLockupData(db ethdb.Database, ownerContract, beneficiaryMiner common.Address, location common.Location, logger *log.Logger) (map[string]map[string][]interface{}, error) {
	prefix := append(rawdb.CoinbaseLockupPrefix, ownerContract.Bytes()...)
	prefix = append(prefix, beneficiaryMiner.Bytes()...)
	it := db.NewIterator(prefix, nil)
	batch := db.NewBatch()
	defer it.Release()
	epochToLockupByteToLockupMap := make(map[string]map[string][]interface{})
	for it.Next() {
		key := it.Key()
		if len(key) != rawdb.CoinbaseLockupKeyLength {
			continue
		}
		_, _, lockupByte, epoch, err := rawdb.ReverseCoinbaseLockupKey(key, location)
		if err != nil {
			return nil, err
		}
		balance, trancheUnlockHeight, elements, delegate := rawdb.ReadCoinbaseLockup(db, batch, ownerContract, beneficiaryMiner, lockupByte, epoch)
		if trancheUnlockHeight == 0 {
			logger.Errorf("lockup is empty: ownerContract=%v, beneficiaryMiner=%v, lockupByte=%v, epoch=%v", ownerContract, beneficiaryMiner, lockupByte, epoch)
		}
		jsonLockup := map[string]interface{}{
			"balance":             balance,
			"trancheUnlockHeight": trancheUnlockHeight,
			"elements":            elements,
			"delegate":            delegate,
		}
		if _, ok := epochToLockupByteToLockupMap[fmt.Sprintf("%d", epoch)]; !ok {
			epochToLockupByteToLockupMap[fmt.Sprintf("%d", epoch)] = make(map[string][]interface{})
		}
		epochToLockupByteToLockupMap[fmt.Sprintf("%d", epoch)][fmt.Sprintf("%d", lockupByte)] = append(epochToLockupByteToLockupMap[fmt.Sprintf("%d", epoch)][fmt.Sprintf("%d", lockupByte)], jsonLockup)
	}
	return epochToLockupByteToLockupMap, nil
}

func ClaimCoinbaseLockup(evm *EVM, ownerContract common.Address, gas *uint64, input []byte) error { // Ensure msg.sender is ownerContract
	// Input should be tightly packed 53 bytes
	if len(input) != 53 {
		return errors.New("input length is not 33 bytes")
	}

	// Extract beneficiaryMiner
	beneficiaryMiner := common.BytesToAddress(input[:20], evm.chainConfig.Location)
	// Extract to address
	to := common.BytesToAddress(input[20:40], evm.chainConfig.Location)
	// Extract lockupByte
	lockupByte := input[40]

	// Extract epoch
	epoch := binary.BigEndian.Uint32(input[41:45])

	// Extract etxGasLimit
	etxGasLimit := binary.BigEndian.Uint64(input[45:53])

	if *gas < etxGasLimit {
		return ErrOutOfGas
	}
	*gas -= etxGasLimit

	_, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return err
	}
	_, err = beneficiaryMiner.InternalAddress()
	if err != nil {
		return err
	}
	latestEpoch := uint32((evm.Context.BlockNumber.Uint64() / params.CoinbaseEpochBlocks) + 1)
	if epoch >= latestEpoch {
		return fmt.Errorf("epoch is not less than the latest epoch: %d >= %d", epoch, latestEpoch)
	}
	if beneficiaryMiner.IsInQiLedgerScope() && to.IsInQuaiLedgerScope() || beneficiaryMiner.IsInQuaiLedgerScope() && to.IsInQiLedgerScope() {
		return errors.New("beneficiaryMiner and to cannot be in different ledgers")
	}

	balance, trancheUnlockHeight, elements, delegate := rawdb.ReadCoinbaseLockup(evm.StateDB.UnderlyingDatabase(), evm.Batch, ownerContract, beneficiaryMiner, lockupByte, epoch)
	if trancheUnlockHeight == 0 {
		return errors.New("no lockup to claim")
	}
	if trancheUnlockHeight > uint32(evm.Context.BlockNumber.Uint64()) {
		return errors.New("tranche is not unlocked yet")
	}
	if elements == 0 {
		return errors.New("no lockup to claim")
	}
	deletedCoinbaseLockupHash := types.CoinbaseLockupHash(ownerContract, beneficiaryMiner, delegate, lockupByte, epoch, balance, trancheUnlockHeight, elements)
	coinbaseLockupKey := rawdb.DeleteCoinbaseLockup(evm.Batch, ownerContract, beneficiaryMiner, lockupByte, epoch)

	evm.ETXCacheLock.RLock()
	index := len(evm.ETXCache)
	evm.ETXCacheLock.RUnlock()
	if index > math.MaxUint16 {
		return fmt.Errorf("CreateETX overflow error: too many ETXs in cache")
	}

	externalTx := types.ExternalTx{Value: balance, To: &to, Sender: ownerContract, EtxType: uint64(types.CoinbaseLockupType), OriginatingTxHash: evm.Hash, ETXIndex: uint16(index), Gas: etxGasLimit}

	evm.ETXCacheLock.Lock()
	evm.ETXCache = append(evm.ETXCache, types.NewTx(&externalTx))
	evm.CoinbaseDeletedHashes = append(evm.CoinbaseDeletedHashes, &deletedCoinbaseLockupHash)
	if err := rawdb.WriteCoinbaseLockupToMap(evm.CoinbasesDeleted, coinbaseLockupKey, balance, trancheUnlockHeight, elements, delegate); err != nil {
		evm.ETXCacheLock.Unlock()
		return err
	}
	evm.ETXCacheLock.Unlock()
	return nil
}

// AddNewLock adds a new locked balance to the lockup contract
func AddNewLock(statedb StateDB, batch ethdb.Batch, ownerContract common.Address, beneficiaryMiner common.Address, delegate common.Address, sender common.InternalAddress, lockupByte byte, unlockHeight uint64, epoch uint32, value *big.Int, location common.Location, logger *logrus.Logger, parentHash common.Hash, log_ bool) (bool, []byte, []byte, common.Hash, common.Hash, error) {
	_, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return false, nil, nil, common.Hash{}, common.Hash{}, err
	}
	_, err = beneficiaryMiner.InternalAddress()
	if err != nil {
		return false, nil, nil, common.Hash{}, common.Hash{}, err
	}
	if sender != common.OneInternal(location) {
		return false, nil, nil, common.Hash{}, common.Hash{}, errors.New("sender is not the correct internal address")
	}
	if value.Sign() == -1 || value.Sign() == 0 {
		return false, nil, nil, common.Hash{}, common.Hash{}, fmt.Errorf("value is not positive: %v", value.String())
	}
	balance, trancheUnlockHeight, elements, oldDelegate := rawdb.ReadCoinbaseLockup(statedb.UnderlyingDatabase(), batch, ownerContract, beneficiaryMiner, lockupByte, epoch) // delegate can be changed every update if the miner chooses

	oldCoinbaseLockupHash := types.CoinbaseLockupHash(ownerContract, beneficiaryMiner, oldDelegate, lockupByte, epoch, balance, trancheUnlockHeight, elements)
	if trancheUnlockHeight != 0 && unlockHeight < uint64(trancheUnlockHeight) {
		return false, nil, nil, common.Hash{}, common.Hash{}, errors.New("new unlock height is less than the current tranche unlock height, math is broken")
	}
	if epoch == 0 && trancheUnlockHeight != 0 {
		return false, nil, nil, common.Hash{}, common.Hash{}, errors.New("epoch is 0 but trancheUnlockHeight is not")
	}

	deleted := true
	var oldLockupData []byte
	if trancheUnlockHeight == 0 {
		// New epoch: create new lockup tranche, don't change previous one
		if epoch+1 > math.MaxUint32 {
			return false, nil, nil, common.Hash{}, common.Hash{}, errors.New("epoch overflow")
		}
		epochUnlockHeight := unlockHeight - unlockHeight%params.CoinbaseEpochBlocks
		elements = 0
		balance = new(big.Int)
		trancheUnlockHeight = uint32(epochUnlockHeight) // TODO: ensure overflow is acceptable here
		oldCoinbaseLockupHash = common.Hash{}
		if log_ {
			logger.Info("State processor Rotated epoch: ", " owner: ", ownerContract, " miner: ", beneficiaryMiner, " epoch: ", epoch, " blockHash: ", parentHash)
		} else {
			logger.Info("Worker Rotated epoch: ", " owner: ", ownerContract, " miner: ", beneficiaryMiner, " epoch: ", epoch, " blockHash: ", parentHash)
		}
		deleted = false
	} else {
		oldLockupData, err = rawdb.WriteCoinbaseLockupToSlice(balance, trancheUnlockHeight, elements, delegate)
		if err != nil {
			return false, nil, nil, common.Hash{}, common.Hash{}, err
		}
	}

	elements++
	balance.Add(balance, value)

	key, err := rawdb.WriteCoinbaseLockup(batch, ownerContract, beneficiaryMiner, lockupByte, epoch, balance, trancheUnlockHeight, elements, delegate)
	if err != nil {
		return false, nil, nil, common.Hash{}, common.Hash{}, err
	}

	newCoinbaseLockupHash := types.CoinbaseLockupHash(ownerContract, beneficiaryMiner, delegate, lockupByte, epoch, balance, trancheUnlockHeight, elements)
	if log_ {
		logger.Info("State processor Added new lockup: ", " contract: ", ownerContract, " miner: ", beneficiaryMiner, " epoch: ", epoch, " balance: ", balance.String(), " value: ", value.String(), " trancheUnlockHeight: ", trancheUnlockHeight, " elements: ", elements, " lockupByte: ", lockupByte, " blockHash: ", parentHash)
	} else {
		logger.Info("Worker Added new lockup: ", " contract: ", ownerContract, " miner: ", beneficiaryMiner, " epoch: ", epoch, " balance: ", balance.String(), " value: ", value.String(), " trancheUnlockHeight: ", trancheUnlockHeight, " elements: ", elements, " lockupByte: ", lockupByte, " blockHash: ", parentHash)
	}
	return deleted, oldLockupData, key, oldCoinbaseLockupHash, newCoinbaseLockupHash, nil
}

// UnwrapQi is called by a smart contract that owns wrapped Qi to unwrap it for real Qi UTXOs
// It deducts the requested Qi balance from the contract's balance and creates an external transaction to the beneficiary
// It is the responsibility of the contract to ensure solvency in its underyling wrapped Qi balance
func UnwrapQi(evm *EVM, ownerContract common.Address, gas *uint64, input []byte) error {
	// input is tightly packed 20 bytes for Qi beneficiary, 32 bytes for value and 8 bytes for etxGasLimit
	if len(input) != 60 {
		return errors.New("input length is not 36 bytes")
	}
	beneficiaryQi := common.BytesToAddress(input[:20], evm.chainConfig.Location)
	value := new(big.Int).SetBytes(input[20:52])
	etxGasLimit := binary.BigEndian.Uint64(input[52:60])

	if *gas < etxGasLimit {
		return ErrOutOfGas
	}
	*gas -= etxGasLimit

	lockupContractAddress := LockupContractAddresses[[2]byte{evm.chainConfig.Location[0], evm.chainConfig.Location[1]}]
	lockupContractAddressInternal, err := lockupContractAddress.InternalAndQuaiAddress()
	if err != nil {
		return err
	}

	_, err = beneficiaryQi.InternalAndQiAddress()
	if err != nil {
		return err
	}
	ownerContractInternal, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return err
	}
	ownerContractHash := common.BytesToHash(ownerContractInternal[:])

	balanceHash := evm.StateDB.GetState(lockupContractAddressInternal, ownerContractHash)
	if balanceHash == (common.Hash{}) {
		return errors.New("no wrapped Qi balance in contract to unwrap")
	}
	balanceBig := balanceHash.Big()
	if balanceBig.Cmp(value) == -1 {
		return fmt.Errorf("not enough wrapped Qi to unwrap: have %v want %v", balanceBig, value)
	}
	balanceBig.Sub(balanceBig, value)
	evm.StateDB.SetState(lockupContractAddressInternal, ownerContractHash, common.BigToHash(balanceBig))

	evm.ETXCacheLock.RLock()
	index := len(evm.ETXCache)
	evm.ETXCacheLock.RUnlock()
	if index > math.MaxUint16 {
		return fmt.Errorf("CreateETX overflow error: too many ETXs in cache")
	}

	externalTx := types.ExternalTx{Value: value, To: &beneficiaryQi, Sender: ownerContract, EtxType: uint64(types.UnwrapQiType), OriginatingTxHash: evm.Hash, ETXIndex: uint16(index), Gas: etxGasLimit}
	log.Global.Infof("Unwrapped Qi: %v -> %v, value: %v", ownerContract, beneficiaryQi, value)
	evm.ETXCacheLock.Lock()
	evm.ETXCache = append(evm.ETXCache, types.NewTx(&externalTx))
	evm.ETXCacheLock.Unlock()
	return nil
}

// WrapQi is called by the state processor to process an inbound Qi wrapping ETX
// It stores the wrapped Qi balance in the lockup contract keyed with the contract address and provided Quai beneficiary address
// To accept the deposit, the smart contract must call the ClaimQiDeposit function on the precompile
func WrapQi(statedb StateDB, ownerContract, beneficiary common.Address, sender common.InternalAddress, value *big.Int, location common.Location) error {
	ownerContractInternal, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return err
	}
	beneficiaryInternal, err := beneficiary.InternalAndQuaiAddress()
	if err != nil {
		return err
	}
	if sender != common.OneInternal(location) {
		return errors.New("sender is not the correct internal address")
	}
	lockupContractAddress := LockupContractAddresses[[2]byte{location[0], location[1]}]
	lockupContractAddressInternal, err := lockupContractAddress.InternalAndQuaiAddress()
	if err != nil {
		return err
	}

	if value.Sign() == -1 {
		return errors.New("negative value")
	}
	if value.Sign() == 0 {
		return nil
	}
	wrappedQiKey := common.Hash{}
	copy(wrappedQiKey[:16], ownerContractInternal[:16])
	copy(wrappedQiKey[16:], beneficiaryInternal[:16])
	balanceHash := statedb.GetState(lockupContractAddressInternal, wrappedQiKey)
	if (balanceHash == common.Hash{}) {
		statedb.SetState(lockupContractAddressInternal, wrappedQiKey, common.BigToHash(value))
	} else {
		balanceBig := balanceHash.Big()
		balanceBig.Add(balanceBig, value)
		statedb.SetState(lockupContractAddressInternal, wrappedQiKey, common.BigToHash(balanceBig))
	}
	statedb.Finalize(true)
	log.Global.Infof("Wrapped Qi: %v -> %v, value: %v", ownerContract, beneficiary, value)
	return nil
}

// ClaimQiDeposit is called by the owner smart contract to claim a wrapped Qi deposit
// It adds the wrapped Qi balance to the smart contract's Wrapped Qi balance
// The contract should then mint the equivalent amount of Wrapped Qi tokens to the Quai beneficiary
func ClaimQiDeposit(evm *EVM, ownerContract common.Address, gas *uint64, input []byte) ([]byte, error) {
	// input is tightly packed 20 bytes for Quai owner
	if len(input) != 20 {
		return nil, errors.New("input length is not 20 bytes")
	}
	quaiOwner := common.BytesToAddress(input, evm.chainConfig.Location)
	lockupContractAddress := LockupContractAddresses[[2]byte{evm.chainConfig.Location[0], evm.chainConfig.Location[1]}]
	lockupContractAddressInternal, err := lockupContractAddress.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	ownerContractInternal, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	ownerQuaiInternal, err := quaiOwner.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}

	wrappedQiKey := common.Hash{}
	copy(wrappedQiKey[:16], ownerContractInternal[:16])
	copy(wrappedQiKey[16:], ownerQuaiInternal[:16])
	balanceHash := evm.StateDB.GetState(lockupContractAddressInternal, wrappedQiKey)
	if balanceHash == (common.Hash{}) {
		return nil, errors.New("no wrapped Qi balance to claim")
	}
	evm.StateDB.SetState(lockupContractAddressInternal, wrappedQiKey, common.Hash{})

	ownerContractHash := common.BytesToHash(ownerContractInternal[:])
	ownerContractBalanceHash := evm.StateDB.GetState(lockupContractAddressInternal, ownerContractHash)
	if ownerContractBalanceHash == (common.Hash{}) {
		evm.StateDB.SetState(lockupContractAddressInternal, ownerContractHash, balanceHash)
	} else {
		ownerContractBalance := ownerContractBalanceHash.Big()
		ownerContractBalance.Add(ownerContractBalance, balanceHash.Big())
		evm.StateDB.SetState(lockupContractAddressInternal, ownerContractHash, common.BigToHash(ownerContractBalance))
	}
	log.Global.Infof("Claimed Qi Deposit: %v -> %v, value: %v", ownerContract, quaiOwner, balanceHash.Big().String())
	return balanceHash[:], nil
}

func GetWrappedQiDeposit(statedb StateDB, ownerContract, quaiOwner common.Address, location common.Location) (*big.Int, error) {
	lockupContractAddress := LockupContractAddresses[[2]byte{location[0], location[1]}]
	lockupContractAddressInternal, err := lockupContractAddress.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	ownerContractInternal, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	ownerQuaiInternal, err := quaiOwner.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}

	wrappedQiKey := common.Hash{}
	copy(wrappedQiKey[:16], ownerContractInternal[:16])
	copy(wrappedQiKey[16:], ownerQuaiInternal[:16])
	balanceHash := statedb.GetState(lockupContractAddressInternal, wrappedQiKey)
	if balanceHash == (common.Hash{}) {
		return nil, errors.New("no wrapped Qi balance")
	}
	return balanceHash.Big(), nil
}

func GetWrappedQiContractBalance(statedb StateDB, ownerContract common.Address, location common.Location) (*big.Int, error) {
	lockupContractAddress := LockupContractAddresses[[2]byte{location[0], location[1]}]
	lockupContractAddressInternal, err := lockupContractAddress.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	ownerContractInternal, err := ownerContract.InternalAndQuaiAddress()
	if err != nil {
		return nil, err
	}
	ownerContractHash := common.BytesToHash(ownerContractInternal[:])

	balanceHash := statedb.GetState(lockupContractAddressInternal, ownerContractHash)
	if balanceHash == (common.Hash{}) {
		return nil, errors.New("no wrapped Qi balance")
	}
	return balanceHash.Big(), nil
}
