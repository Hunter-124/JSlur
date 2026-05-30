package jobs

import "strings"

// usStates maps lowercase state abbreviations and full names so a location
// string can be recognised as US.
var usStates = func() map[string]bool {
	pairs := [][2]string{
		{"al", "alabama"}, {"ak", "alaska"}, {"az", "arizona"}, {"ar", "arkansas"},
		{"ca", "california"}, {"co", "colorado"}, {"ct", "connecticut"}, {"de", "delaware"},
		{"fl", "florida"}, {"ga", "georgia"}, {"hi", "hawaii"}, {"id", "idaho"},
		{"il", "illinois"}, {"in", "indiana"}, {"ia", "iowa"}, {"ks", "kansas"},
		{"ky", "kentucky"}, {"la", "louisiana"}, {"me", "maine"}, {"md", "maryland"},
		{"ma", "massachusetts"}, {"mi", "michigan"}, {"mn", "minnesota"}, {"ms", "mississippi"},
		{"mo", "missouri"}, {"mt", "montana"}, {"ne", "nebraska"}, {"nv", "nevada"},
		{"nh", "new hampshire"}, {"nj", "new jersey"}, {"nm", "new mexico"}, {"ny", "new york"},
		{"nc", "north carolina"}, {"nd", "north dakota"}, {"oh", "ohio"}, {"ok", "oklahoma"},
		{"or", "oregon"}, {"pa", "pennsylvania"}, {"ri", "rhode island"}, {"sc", "south carolina"},
		{"sd", "south dakota"}, {"tn", "tennessee"}, {"tx", "texas"}, {"ut", "utah"},
		{"vt", "vermont"}, {"va", "virginia"}, {"wa", "washington"}, {"wv", "west virginia"},
		{"wi", "wisconsin"}, {"wy", "wyoming"}, {"dc", "district of columbia"},
	}
	m := map[string]bool{}
	for _, p := range pairs {
		m[p[0]] = true
		m[p[1]] = true
	}
	return m
}()

// isUSLocation reports whether a location string looks like it is in the US.
func isUSLocation(loc string) bool {
	l := strings.ToLower(strings.TrimSpace(loc))
	if l == "" {
		return false
	}
	if strings.Contains(l, "united states") || strings.Contains(l, "usa") ||
		strings.Contains(l, "u.s.") || strings.Contains(l, ", us") {
		return true
	}
	// Tokenise on commas/spaces/slashes and look for a state token.
	for _, tok := range strings.FieldsFunc(l, func(r rune) bool {
		return r == ',' || r == ' ' || r == '/' || r == '|'
	}) {
		if usStates[strings.TrimSpace(tok)] {
			return true
		}
	}
	// Full state names can be multi-word; check substring for those.
	for name := range usStates {
		if len(name) > 4 && strings.Contains(l, name) {
			return true
		}
	}
	return false
}
