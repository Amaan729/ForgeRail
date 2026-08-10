package ledger_test

import (
	"testing"

	"github.com/Amaan729/ForgeRail/internal/ledger"
	"github.com/Amaan729/ForgeRail/internal/ledger/ledgertest"
)

func TestMemStore(t *testing.T) {
	ledgertest.Run(t, func(t *testing.T) ledger.Store { return ledger.NewMemStore() })
}
