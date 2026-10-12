package profile

import (
	"tux-migration-tool/internal/common"
)

// goitem is profile #1: the reference Go service conventions (sqlx, tx
// variants, pkg/services/<svc> layout) — today's gen/convert constants
// extracted verbatim (P1). The zero-config default.
type goitem struct{}

// Default returns the zero-config profile (goitem). The config selector
// ("target.profile") routes nowhere today — the registry seam was deleted
// as dead API (engine-wiring audit Tier-2); a second target reintroduces
// selection deliberately, with its consumer.
func Default() Profile {
	return goitem{}
}

// id implements Profile.ID for goitem.
func (goitem) ID() string { return "goitem" }

// Naming implements Profile.Naming with the gen conventions (verbatim
// from gen.requestType/responseType/RowName/lowerFirst — P1 re-points).
func (goitem) Naming() Naming {
	return Naming{
		Request:  func(endpoint string) string { return endpoint + "Request" },
		Response: func(endpoint string) string { return endpoint + "Response" },
		Row: func(methodName string) string {
			for _, verb := range []string{"Get", "Insert", "Update", "Delete", "Merge"} {
				if n, ok := cutPrefix(methodName, verb); ok {
					return n
				}
			}
			return methodName
		},
		Receiver: common.LowerFirst,
	}
}

// DB implements Profile.DB with the sqlx/tx-variant contract (decision 27).
func (goitem) DB() DBRules {
	return DBRules{
		StoreReceiver: common.GoStoreCallPrefix(),
		TxVariants: func(queryType string) bool {
			switch queryType {
			case "INSERT", "UPDATE", "DELETE":
				return true
			}
			return false
		},
	}
}

// cutPrefix reports s with prefix removed.
func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}
