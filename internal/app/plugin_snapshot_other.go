//go:build !darwin

package app

import "os"

func checkPluginSnapshotSource(_ *os.File) error { return nil }
