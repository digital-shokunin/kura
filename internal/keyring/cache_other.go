//go:build !darwin

package keyring

func platformSecrets() (Secrets, error) { return defaultFileSecrets() }
