//go:build darwin

package keyring

import (
	"errors"

	"github.com/keybase/go-keychain"
)

// Keychain stores secrets as generic passwords in the login keychain. Items
// are not synchronizable, so they never go to iCloud Keychain. Access is
// bound to the code signature of the binary that created the item, which is
// why kura and git-remote-kura are one signed binary.
type Keychain struct{}

func platformSecrets() (Secrets, error) { return Keychain{}, nil }

func (Keychain) Name() string { return "macOS login Keychain" }

func query(service, account string) keychain.Item {
	q := keychain.NewItem()
	q.SetSecClass(keychain.SecClassGenericPassword)
	q.SetService(service)
	q.SetAccount(account)
	return q
}

func (Keychain) Get(service, account string) ([]byte, error) {
	q := query(service, account)
	q.SetMatchLimit(keychain.MatchLimitOne)
	q.SetReturnData(true)
	res, err := keychain.QueryItem(q)
	if errors.Is(err, keychain.ErrorItemNotFound) || (err == nil && len(res) == 0) {
		return nil, ErrNotCached
	}
	if err != nil {
		return nil, err
	}
	return res[0].Data, nil
}

func (Keychain) Set(service, account string, data []byte) error {
	item := query(service, account)
	item.SetLabel(service + " " + account)
	item.SetDescription("kura encrypted git remote")
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetData(data)
	err := keychain.AddItem(item)
	if errors.Is(err, keychain.ErrorDuplicateItem) {
		update := keychain.NewItem()
		update.SetData(data)
		return keychain.UpdateItem(query(service, account), update)
	}
	return err
}

func (Keychain) Delete(service, account string) error {
	err := keychain.DeleteItem(query(service, account))
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil
	}
	return err
}
