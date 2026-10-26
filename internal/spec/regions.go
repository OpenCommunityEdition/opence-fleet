package spec

// Region is a region a profile's servers can say they are in: its value
// (server.region, what the server browser shows) and its words.
type Region struct {
	Value, Name string
}

// Regions are the regions a profile chooses from: the master servers'
// /apply offers the same (opence-master-server's discord.Regions).
var Regions = []Region{
	{"eu", "Europe"},
	{"na-east", "North America (east)"},
	{"na-west", "North America (west)"},
	{"sa", "South America"},
	{"asia", "Asia"},
	{"oce", "Oceania"},
	{"me", "Middle East"},
	{"africa", "Africa"},
}

// KnownRegion tells whether a region is one of Regions.
func KnownRegion(value string) bool {
	for _, r := range Regions {
		if r.Value == value {
			return true
		}
	}
	return false
}
