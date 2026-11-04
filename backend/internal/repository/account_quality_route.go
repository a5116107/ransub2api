package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Share the forwarding identity, not updated_at: usage, fault protection and
// manual concurrency may change while a valid model probe is running.
func accountRouteFingerprint(ctx context.Context, tx *sql.Tx, accountID int64, lock bool) ([32]byte, error) {
	seen := map[int64]bool{}
	accountLock, proxyLock := "", ""
	if lock {
		accountLock, proxyLock = " FOR UPDATE", " FOR SHARE"
	}
	var chain []byte
	for {
		if seen[accountID] {
			return [32]byte{}, fmt.Errorf("quality probe account parent cycle")
		}
		seen[accountID] = true
		account := &service.Account{}
		var credentials, extra []byte
		if err := tx.QueryRowContext(ctx, `SELECT platform,type,COALESCE(credentials,'{}'::jsonb),COALESCE(extra,'{}'::jsonb),proxy_id,parent_account_id
 FROM accounts WHERE id=$1 AND deleted_at IS NULL`+accountLock, accountID).Scan(
			&account.Platform, &account.Type, &credentials, &extra, &account.ProxyID, &account.ParentAccountID); err != nil {
			return [32]byte{}, err
		}
		if err := json.Unmarshal(credentials, &account.Credentials); err != nil {
			return [32]byte{}, err
		}
		if err := json.Unmarshal(extra, &account.Extra); err != nil {
			return [32]byte{}, err
		}
		if account.ProxyID != nil {
			account.Proxy = &service.Proxy{ID: *account.ProxyID}
			if err := tx.QueryRowContext(ctx, `SELECT protocol,host,port,COALESCE(username,''),COALESCE(password,'')
 FROM proxies WHERE id=$1 AND deleted_at IS NULL`+proxyLock, *account.ProxyID).Scan(
				&account.Proxy.Protocol, &account.Proxy.Host, &account.Proxy.Port, &account.Proxy.Username, &account.Proxy.Password); err != nil {
				return [32]byte{}, err
			}
		}
		fingerprint := service.AccountRouteFingerprint(account)
		if len(chain) == 0 && account.ParentAccountID == nil {
			return fingerprint, nil
		}
		chain = append(chain, fingerprint[:]...)
		if account.ParentAccountID == nil {
			return sha256.Sum256(chain), nil
		}
		accountID = *account.ParentAccountID
	}
}
