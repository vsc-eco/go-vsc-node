//go:build evm_devnet

// The devnet USDC: MockUSDC (contracts/mock-usdc, Circle's transfer rules)
// deployed on anvil, and the L1 calls the USDC wave makes.
package devnet

import (
	"context"
	"crypto/ecdsa"
	_ "embed"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	ethcommon "github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
)

//go:embed contracts/mock-usdc/MockUSDC.bin
var mockUSDCBin string

// SendTx signs and sends an EIP-1559 tx from key and returns its receipt,
// whatever its status. A nil `to` deploys data as a contract.
func (a *Anvil) SendTx(ctx context.Context, key *ecdsa.PrivateKey, to *ethcommon.Address, data []byte, gas uint64) (*ethtypes.Receipt, error) {
	nonce, err := a.cli.PendingNonceAt(ctx, cryptoPubkeyAddr(key))
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	tx, err := ethtypes.SignTx(ethtypes.NewTx(&ethtypes.DynamicFeeTx{
		ChainID: a.ChainID, Nonce: nonce, GasTipCap: big.NewInt(1e9), GasFeeCap: big.NewInt(20e9),
		Gas: gas, To: to, Value: new(big.Int), Data: data,
	}), ethtypes.LatestSignerForChainID(a.ChainID), key)
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}
	if err := a.cli.SendTransaction(ctx, tx); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	return a.waitReceipt(ctx, tx.Hash(), 30*time.Second)
}

// DeployMockUSDC deploys a MockUSDC owned (and minted) by key.
func (a *Anvil) DeployMockUSDC(ctx context.Context, key *ecdsa.PrivateKey) (ethcommon.Address, error) {
	code := ethcommon.FromHex(strings.TrimSpace(mockUSDCBin))
	rc, err := a.SendTx(ctx, key, nil, code, 3_000_000)
	if err != nil {
		return ethcommon.Address{}, err
	}
	if rc.Status != ethtypes.ReceiptStatusSuccessful || rc.ContractAddress == (ethcommon.Address{}) {
		return ethcommon.Address{}, fmt.Errorf("MockUSDC deploy failed: status %d", rc.Status)
	}
	return rc.ContractAddress, nil
}

// usdcCall encodes a call to sig with address and *big.Int arguments, each
// one 32-byte word.
func usdcCall(sig string, args ...any) []byte {
	out := ethCrypto.Keccak256([]byte(sig))[:4]
	for _, a := range args {
		switch v := a.(type) {
		case ethcommon.Address:
			out = append(out, ethcommon.LeftPadBytes(v.Bytes(), 32)...)
		case *big.Int:
			out = append(out, ethcommon.LeftPadBytes(v.Bytes(), 32)...)
		default:
			panic(fmt.Sprintf("usdcCall: unsupported argument %T", a))
		}
	}
	return out
}

// ERC20BalanceOf is holder's balance of token at the latest block.
func (a *Anvil) ERC20BalanceOf(ctx context.Context, token, holder ethcommon.Address) (*big.Int, error) {
	out, err := a.cli.CallContract(ctx, ethereum.CallMsg{To: &token, Data: usdcCall("balanceOf(address)", holder)}, nil)
	if err != nil {
		return nil, err
	}
	if len(out) != 32 {
		return nil, fmt.Errorf("balanceOf returned %d bytes", len(out))
	}
	return new(big.Int).SetBytes(out), nil
}

// SetMockUSDCBalance overwrites holder's MockUSDC balance (balanceOf is
// storage slot 0) without a transfer: a custody shortfall, for the alarms.
func (a *Anvil) SetMockUSDCBalance(ctx context.Context, token, holder ethcommon.Address, v *big.Int) error {
	slot := ethCrypto.Keccak256Hash(ethcommon.LeftPadBytes(holder.Bytes(), 32), make([]byte, 32))
	return a.rpc.CallContext(ctx, nil, "anvil_setStorageAt", token.Hex(), slot.Hex(), ethcommon.BigToHash(v).Hex())
}

// freeLocalPort is a TCP port free on 127.0.0.1 right now.
func freeLocalPort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port), nil
}
