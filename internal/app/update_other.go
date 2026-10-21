//go:build !darwin && !linux

package app

import "errors"

func preflightUpdateTarget(string) error {
	return errors.New("self-update is supported only on darwin/linux amd64/arm64")
}
