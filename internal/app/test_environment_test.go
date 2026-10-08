package app

import (
	"meldra/internal/testenv"
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(testenv.Run(m.Run)) }
