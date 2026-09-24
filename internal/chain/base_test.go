package chain

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestTransferSelector(t *testing.T) {
	want := crypto.Keccak256([]byte("transfer(address,uint256)"))[:4]
	if !bytes.Equal(transferSelector, want) {
		t.Fatalf("selector = %x, want %x", transferSelector, want)
	}
}

func TestTransferCalldata(t *testing.T) {
	to := common.HexToAddress("0x00000000000000000000000000000000DeaDBeef")
	data := transferCalldata(to, big.NewInt(1_500_000)) // 1.5 USDC
	if len(data) != 68 {
		t.Fatalf("len = %d, want 68", len(data))
	}
	if got := common.BytesToAddress(data[4:36]); got != to {
		t.Errorf("address = %s, want %s", got, to)
	}
	if got := new(big.Int).SetBytes(data[36:68]); got.Int64() != 1_500_000 {
		t.Errorf("amount = %s", got)
	}
}

func TestConfirmations(t *testing.T) {
	cases := []struct{ head, mined, want uint64 }{
		{100, 100, 1},
		{102, 100, 3},
		{99, 100, 0}, // node behind the block we got the receipt from
	}
	for _, c := range cases {
		if got := confirmations(c.head, c.mined); got != c.want {
			t.Errorf("confirmations(%d, %d) = %d, want %d", c.head, c.mined, got, c.want)
		}
	}
}

func TestUSDCAddressesAreValid(t *testing.T) {
	for _, a := range []string{USDCBaseMainnet, USDCBaseSepolia} {
		if !common.IsHexAddress(a) {
			t.Errorf("%s is not a valid address", a)
		}
	}
}
