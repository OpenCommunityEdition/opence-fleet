package fleet_test

import "github.com/OpenCommunityEdition/opence-fleet/internal/store/db"

func dbAuditAll() db.ListAuditParams { return db.ListAuditParams{Lim: 100000} }
