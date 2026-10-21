package app

import (
	"meldra/internal/testenv"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("MELDRA_DISABLE_UPDATE_CHECK", "1")
	os.Exit(testenv.Run(m.Run))
}
