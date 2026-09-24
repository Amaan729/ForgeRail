package chain

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Amaan729/ForgeRail/internal/money"
)

// Circle's native USDC contracts on Base.
const (
	ChainIDBaseMainnet = 8453
	ChainIDBaseSepolia = 84532

	USDCBaseMainnet = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	USDCBaseSepolia = "0x036CbD53842c5426634e7929541eC2318f3dCF7e"
)

// transfer(address,uint256)
var transferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}

type BaseConfig struct {
	RPCURL        string
	ChainID       int64
	TokenAddress  string
	PrivateKeyHex string // hot wallet key. Testnet only please.
	Confirmations uint64
}

// Base sends USDC from a single hot wallet using EIP-1559 transactions.
//
// Nonces are tracked in memory after the first lookup, so only run one
// instance of this per wallet. If a signed tx is never broadcast the nonce is
// burned and later txs get stuck until it is filled; fine for a testnet
// project, not fine for production.
type Base struct {
	rpc     *ethclient.Client
	key     *ecdsa.PrivateKey
	from    common.Address
	token   common.Address
	chainID *big.Int
	signer  types.Signer
	confs   uint64

	mu        sync.Mutex
	nextNonce *uint64
}

var _ Client = (*Base)(nil)

func DialBase(ctx context.Context, cfg BaseConfig) (*Base, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.PrivateKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("chain: bad private key: %w", err)
	}
	if !common.IsHexAddress(cfg.TokenAddress) {
		return nil, fmt.Errorf("chain: bad token address %q", cfg.TokenAddress)
	}
	rpc, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return nil, err
	}
	id, err := rpc.ChainID(ctx)
	if err != nil {
		rpc.Close()
		return nil, fmt.Errorf("chain: eth_chainId: %w", err)
	}
	if cfg.ChainID != 0 && id.Int64() != cfg.ChainID {
		rpc.Close()
		return nil, fmt.Errorf("chain: rpc is chain %d, expected %d", id.Int64(), cfg.ChainID)
	}
	confs := cfg.Confirmations
	if confs == 0 {
		confs = 3
	}
	return &Base{
		rpc:     rpc,
		key:     key,
		from:    crypto.PubkeyToAddress(key.PublicKey),
		token:   common.HexToAddress(cfg.TokenAddress),
		chainID: id,
		signer:  types.LatestSignerForChainID(id),
		confs:   confs,
	}, nil
}

// Address is the hot wallet address. Fund it with testnet ETH + USDC.
func (b *Base) Address() string { return b.from.Hex() }

func (b *Base) Close() { b.rpc.Close() }

func (b *Base) PrepareUSDCTransfer(ctx context.Context, ref, to string, amount money.Amount) (SignedTx, error) {
	if !common.IsHexAddress(to) {
		return SignedTx{}, fmt.Errorf("%w: bad address %q", ErrPermanent, to)
	}
	if amount <= 0 {
		return SignedTx{}, fmt.Errorf("%w: amount must be positive", ErrPermanent)
	}
	data := transferCalldata(common.HexToAddress(to), big.NewInt(amount.Micros()))

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.nextNonce == nil {
		n, err := b.rpc.PendingNonceAt(ctx, b.from)
		if err != nil {
			return SignedTx{}, fmt.Errorf("chain: nonce: %w", err)
		}
		b.nextNonce = &n
	}
	tip, err := b.rpc.SuggestGasTipCap(ctx)
	if err != nil {
		return SignedTx{}, fmt.Errorf("chain: gas tip: %w", err)
	}
	head, err := b.rpc.HeaderByNumber(ctx, nil)
	if err != nil {
		return SignedTx{}, fmt.Errorf("chain: head: %w", err)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	gas, err := b.rpc.EstimateGas(ctx, ethereum.CallMsg{From: b.from, To: &b.token, Data: data})
	if err != nil {
		// estimateGas fails when the call would revert, e.g. the hot wallet
		// doesn't hold enough USDC. Retrying won't fix that.
		return SignedTx{}, fmt.Errorf("%w: estimate gas: %v", ErrPermanent, err)
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   b.chainID,
		Nonce:     *b.nextNonce,
		GasTipCap: tip,
		GasFeeCap: feeCap,
		Gas:       gas * 12 / 10,
		To:        &b.token,
		Data:      data,
	})
	signed, err := types.SignTx(tx, b.signer, b.key)
	if err != nil {
		return SignedTx{}, err
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		return SignedTx{}, err
	}
	*b.nextNonce++
	return SignedTx{Hash: signed.Hash().Hex(), Raw: raw}, nil
}

func (b *Base) Broadcast(ctx context.Context, stx SignedTx) error {
	var tx types.Transaction
	if err := tx.UnmarshalBinary(stx.Raw); err != nil {
		return fmt.Errorf("%w: decode tx: %v", ErrPermanent, err)
	}
	err := b.rpc.SendTransaction(ctx, &tx)
	if err == nil {
		return nil
	}
	// Re-sending a tx the node already has is fine. "nonce too low" usually
	// means our tx was already mined; Receipt() decides what really happened.
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "already known") || strings.Contains(msg, "nonce too low") {
		return nil
	}
	return err
}

func (b *Base) Receipt(ctx context.Context, hash string) (Receipt, error) {
	rc, err := b.rpc.TransactionReceipt(ctx, common.HexToHash(hash))
	if errors.Is(err, ethereum.NotFound) {
		return Receipt{TxHash: hash, Status: TxNotFound}, nil
	}
	if err != nil {
		return Receipt{}, err
	}
	head, err := b.rpc.BlockNumber(ctx)
	if err != nil {
		return Receipt{}, err
	}
	out := Receipt{TxHash: hash, BlockNumber: rc.BlockNumber.Uint64()}
	out.Confirmations = confirmations(head, out.BlockNumber)
	switch {
	case rc.Status == types.ReceiptStatusFailed:
		out.Status = TxReverted
	case out.Confirmations < b.confs:
		out.Status = TxPending
	default:
		out.Status = TxConfirmed
	}
	return out, nil
}

func confirmations(head, mined uint64) uint64 {
	if head < mined {
		return 0
	}
	return head - mined + 1
}

// transferCalldata ABI-encodes transfer(to, amount) by hand; it is the only
// call we make so pulling in the abi package isn't worth it.
func transferCalldata(to common.Address, amount *big.Int) []byte {
	data := make([]byte, 0, 4+32+32)
	data = append(data, transferSelector...)
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)
	return data
}
