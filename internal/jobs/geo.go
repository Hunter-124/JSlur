package jobs

import (
	"math"
	"strconv"
	"strings"

	"autoapply/internal/config"
)

// This file implements an offline geographic radius filter. Several sources
// (The Muse, Remotive, scraped boards) return jobs from anywhere in the US
// regardless of the requested location, so without this the engine would tailor
// résumés for jobs hundreds of miles outside the user's search area. We can't do
// per-job online geocoding cheaply, so we resolve a coarse coordinate for the
// home location and each job location from an embedded gazetteer (state
// centroids + major cities + ZIP→state) and compare great-circle distance.
//
// The filter is a backstop, not a precise geocoder: when a job's city isn't in
// the table we fall back to its state, and a job in the *same* state as the home
// (whose exact city we can't pinpoint) is kept rather than risk dropping a valid
// local posting. A generous buffer is added on top of the radius for the same
// reason. The goal is to drop the gross "wrong region" results, not to litigate
// a 30-vs-25-mile suburb.

type geoLevel int

const (
	geoNone  geoLevel = iota // could not localize at all
	geoState                 // resolved to a state centroid only
	geoCity                  // resolved to a specific city
)

// geoPlace is a resolved location: coordinates plus how precisely we pinned it.
type geoPlace struct {
	lat, lng float64
	state    string // two-letter abbreviation, "" if unknown
	level    geoLevel
}

// stateCentroids maps a two-letter state abbreviation to its approximate
// geographic center (decimal degrees). Used as the coarse fallback when a city
// can't be matched.
var stateCentroids = map[string][2]float64{
	"al": {32.81, -86.79}, "ak": {63.59, -154.49}, "az": {34.17, -111.93}, "ar": {34.97, -92.37},
	"ca": {37.18, -119.47}, "co": {39.06, -105.31}, "ct": {41.60, -72.76}, "de": {39.32, -75.51},
	"fl": {28.63, -82.45}, "ga": {32.68, -83.22}, "hi": {20.29, -156.37}, "id": {44.24, -114.48},
	"il": {40.35, -89.00}, "in": {39.85, -86.26}, "ia": {42.01, -93.21}, "ks": {38.53, -96.73},
	"ky": {37.67, -84.67}, "la": {31.17, -91.87}, "me": {44.69, -69.38}, "md": {39.06, -76.80},
	"ma": {42.23, -71.53}, "mi": {43.33, -84.54}, "mn": {45.69, -93.90}, "ms": {32.74, -89.68},
	"mo": {38.46, -92.29}, "mt": {46.92, -110.45}, "ne": {41.13, -98.27}, "nv": {38.50, -117.02},
	"nh": {43.45, -71.56}, "nj": {40.30, -74.52}, "nm": {34.84, -106.25}, "ny": {42.95, -75.53},
	"nc": {35.63, -79.81}, "nd": {47.53, -99.78}, "oh": {40.39, -82.76}, "ok": {35.57, -96.93},
	"or": {43.94, -120.55}, "pa": {40.88, -77.80}, "ri": {41.68, -71.51}, "sc": {33.86, -80.95},
	"sd": {44.30, -99.44}, "tn": {35.75, -86.69}, "tx": {31.48, -99.33}, "ut": {39.32, -111.68},
	"vt": {44.04, -72.71}, "va": {37.52, -78.85}, "wa": {47.40, -120.55}, "wv": {38.64, -80.62},
	"wi": {44.62, -89.99}, "wy": {42.99, -107.55}, "dc": {38.90, -77.04},
}

// cityCoords maps "city, st" (lowercased) to coordinates for the major US
// population centers — enough to cover where most postings and home locations
// concentrate. Unlisted towns fall back to their state centroid.
var cityCoords = map[string][2]float64{
	"new york, ny": {40.71, -74.01}, "los angeles, ca": {34.05, -118.24}, "chicago, il": {41.88, -87.63},
	"houston, tx": {29.76, -95.37}, "phoenix, az": {33.45, -112.07}, "philadelphia, pa": {39.95, -75.16},
	"san antonio, tx": {29.42, -98.49}, "san diego, ca": {32.72, -117.16}, "dallas, tx": {32.78, -96.80},
	"san jose, ca": {37.34, -121.89}, "austin, tx": {30.27, -97.74}, "jacksonville, fl": {30.33, -81.66},
	"fort worth, tx": {32.76, -97.33}, "columbus, oh": {39.96, -82.99}, "charlotte, nc": {35.23, -80.84},
	"san francisco, ca": {37.77, -122.42}, "indianapolis, in": {39.77, -86.16}, "seattle, wa": {47.61, -122.33},
	"denver, co": {39.74, -104.99}, "washington, dc": {38.90, -77.04}, "boston, ma": {42.36, -71.06},
	"el paso, tx": {31.76, -106.49}, "nashville, tn": {36.16, -86.78}, "detroit, mi": {42.33, -83.05},
	"oklahoma city, ok": {35.47, -97.52}, "portland, or": {45.52, -122.68}, "las vegas, nv": {36.17, -115.14},
	"memphis, tn": {35.15, -90.05}, "louisville, ky": {38.25, -85.76}, "baltimore, md": {39.29, -76.61},
	"milwaukee, wi": {43.04, -87.91}, "albuquerque, nm": {35.08, -106.65}, "tucson, az": {32.22, -110.97},
	"fresno, ca": {36.74, -119.77}, "sacramento, ca": {38.58, -121.49}, "mesa, az": {33.42, -111.83},
	"kansas city, mo": {39.10, -94.58}, "atlanta, ga": {33.75, -84.39}, "omaha, ne": {41.26, -95.93},
	"colorado springs, co": {38.83, -104.82}, "raleigh, nc": {35.78, -78.64}, "miami, fl": {25.76, -80.19},
	"long beach, ca": {33.77, -118.19}, "virginia beach, va": {36.85, -75.98}, "oakland, ca": {37.80, -122.27},
	"minneapolis, mn": {44.98, -93.27}, "tulsa, ok": {36.15, -95.99}, "tampa, fl": {27.95, -82.46},
	"arlington, tx": {32.74, -97.11}, "new orleans, la": {29.95, -90.07}, "wichita, ks": {37.69, -97.34},
	"cleveland, oh": {41.50, -81.69}, "bakersfield, ca": {35.37, -119.02}, "aurora, co": {39.73, -104.83},
	"anaheim, ca": {33.84, -117.91}, "honolulu, hi": {21.31, -157.86}, "santa ana, ca": {33.75, -117.87},
	"riverside, ca": {33.95, -117.40}, "corpus christi, tx": {27.80, -97.40}, "lexington, ky": {38.04, -84.50},
	"henderson, nv": {36.04, -114.98}, "stockton, ca": {37.96, -121.29}, "cincinnati, oh": {39.10, -84.51},
	"pittsburgh, pa": {40.44, -79.99}, "greensboro, nc": {36.07, -79.79}, "lincoln, ne": {40.81, -96.70},
	"anchorage, ak": {61.22, -149.90}, "plano, tx": {33.02, -96.70}, "orlando, fl": {28.54, -81.38},
	"irvine, ca": {33.68, -117.83}, "newark, nj": {40.74, -74.17}, "durham, nc": {35.99, -78.90},
	"st. louis, mo": {38.63, -90.20}, "saint louis, mo": {38.63, -90.20}, "st. paul, mn": {44.95, -93.09},
	"saint paul, mn": {44.95, -93.09}, "toledo, oh": {41.66, -83.56}, "fort wayne, in": {41.08, -85.14},
	"jersey city, nj": {40.73, -74.08}, "chandler, az": {33.31, -111.84}, "madison, wi": {43.07, -89.40},
	"buffalo, ny": {42.89, -78.88}, "lubbock, tx": {33.58, -101.86}, "scottsdale, az": {33.49, -111.93},
	"reno, nv": {39.53, -119.81}, "glendale, az": {33.54, -112.19}, "norfolk, va": {36.85, -76.29},
	"boise, id": {43.62, -116.21}, "richmond, va": {37.54, -77.44}, "baton rouge, la": {30.45, -91.15},
	"spokane, wa": {47.66, -117.43}, "des moines, ia": {41.59, -93.62}, "tacoma, wa": {47.25, -122.44},
	"birmingham, al": {33.52, -86.81}, "rochester, ny": {43.16, -77.61}, "salt lake city, ut": {40.76, -111.89},
	"providence, ri": {41.82, -71.41}, "charleston, sc": {32.78, -79.93}, "columbia, sc": {34.00, -81.03},
	"hartford, ct": {41.76, -72.69}, "new haven, ct": {41.31, -72.93}, "stamford, ct": {41.05, -73.54},
	"bridgeport, ct": {41.18, -73.19}, "syracuse, ny": {43.05, -76.15}, "albany, ny": {42.65, -73.76},
	"dayton, oh": {39.76, -84.19}, "akron, oh": {41.08, -81.52}, "grand rapids, mi": {42.96, -85.67},
	"knoxville, tn": {35.96, -83.92}, "chattanooga, tn": {35.05, -85.31}, "little rock, ar": {34.75, -92.29},
	"jackson, ms": {32.30, -90.18}, "shreveport, la": {32.53, -93.75}, "mobile, al": {30.69, -88.04},
	"montgomery, al": {32.37, -86.30}, "huntsville, al": {34.73, -86.59}, "savannah, ga": {32.08, -81.09},
	"charleston, wv": {38.35, -81.63}, "cheyenne, wy": {41.14, -104.82}, "billings, mt": {45.79, -108.50},
	"fargo, nd": {46.88, -96.79}, "sioux falls, sd": {43.55, -96.70}, "burlington, vt": {44.48, -73.21},
	"portland, me": {43.66, -70.26}, "manchester, nh": {42.99, -71.46}, "wilmington, de": {39.74, -75.55},
	"salem, or": {44.94, -123.04}, "eugene, or": {44.05, -123.09}, "fort lauderdale, fl": {26.12, -80.14},
	"tallahassee, fl": {30.44, -84.28},
}

// stateAbbrev maps both the two-letter code and the full name (lowercased) to
// the canonical two-letter abbreviation.
var stateAbbrev = func() map[string]string {
	m := map[string]string{}
	for _, p := range statePairs {
		m[p[0]] = p[0]
		m[p[1]] = p[0]
	}
	return m
}()

// zipRange maps a contiguous span of 3-digit ZIP prefixes to a state.
type zipRange struct {
	lo, hi int
	state  string
}

// zipRanges covers the standard USPS 3-digit ZIP-prefix → state allocation.
// Used only to derive a home state when the user supplied a ZIP but no state.
var zipRanges = []zipRange{
	{10, 27, "ma"}, {28, 29, "ri"}, {30, 38, "nh"}, {39, 49, "me"}, {50, 59, "vt"},
	{60, 69, "ct"}, {70, 89, "nj"}, {100, 149, "ny"}, {150, 196, "pa"}, {197, 199, "de"},
	{200, 205, "dc"}, {206, 219, "md"}, {220, 246, "va"}, {247, 268, "wv"}, {270, 289, "nc"},
	{290, 299, "sc"}, {300, 319, "ga"}, {320, 349, "fl"}, {350, 369, "al"}, {370, 385, "tn"},
	{386, 397, "ms"}, {398, 399, "ga"}, {400, 427, "ky"}, {430, 459, "oh"}, {460, 479, "in"},
	{480, 499, "mi"}, {500, 528, "ia"}, {530, 549, "wi"}, {550, 567, "mn"}, {570, 577, "sd"},
	{580, 588, "nd"}, {590, 599, "mt"}, {600, 629, "il"}, {630, 658, "mo"}, {660, 679, "ks"},
	{680, 693, "ne"}, {700, 714, "la"}, {716, 729, "ar"}, {730, 749, "ok"}, {750, 799, "tx"},
	{800, 816, "co"}, {820, 831, "wy"}, {832, 838, "id"}, {840, 847, "ut"}, {850, 865, "az"},
	{870, 884, "nm"}, {889, 898, "nv"}, {900, 961, "ca"}, {967, 968, "hi"}, {970, 979, "or"},
	{980, 994, "wa"}, {995, 999, "ak"},
}

// zipState returns the state abbreviation for a ZIP code, or "" if unknown.
func zipState(zip string) string {
	zip = strings.TrimSpace(zip)
	if len(zip) < 3 {
		return ""
	}
	p, err := strconv.Atoi(zip[:3])
	if err != nil {
		return ""
	}
	for _, r := range zipRanges {
		if p >= r.lo && p <= r.hi {
			return r.state
		}
	}
	return ""
}

// haversineMiles returns the great-circle distance in miles between two points.
func haversineMiles(lat1, lng1, lat2, lng2 float64) float64 {
	const earthMiles = 3958.8
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)
	return earthMiles * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// homePlaces resolves the user's configured search location to coordinates.
// Returns nil when there isn't enough to localize (no state/city/known ZIP).
func homePlaces(l config.Location) []geoPlace {
	city := strings.ToLower(strings.TrimSpace(l.City))
	state := stateAbbrev[strings.ToLower(strings.TrimSpace(l.State))]
	if state == "" {
		state = zipState(l.Zip)
	}
	if city != "" && state != "" {
		if c, ok := cityCoords[city+", "+state]; ok {
			return []geoPlace{{c[0], c[1], state, geoCity}}
		}
	}
	if city != "" && state == "" {
		if p, ok := uniqueCity(city); ok {
			return []geoPlace{p}
		}
	}
	if state != "" {
		if sc, ok := stateCentroids[state]; ok {
			return []geoPlace{{sc[0], sc[1], state, geoState}}
		}
	}
	return nil
}

// uniqueCity returns the coordinate for a bare city name only when exactly one
// state in the table has a city by that name (so "chicago" resolves but
// "columbus" / "portland" stay ambiguous and are skipped).
func uniqueCity(city string) (geoPlace, bool) {
	prefix := city + ", "
	var found geoPlace
	n := 0
	for key, c := range cityCoords {
		if strings.HasPrefix(key, prefix) {
			n++
			found = geoPlace{c[0], c[1], strings.TrimPrefix(key, prefix), geoCity}
		}
	}
	if n == 1 {
		return found, true
	}
	return geoPlace{}, false
}

// geoPlaces parses every "City, ST" (or bare state) it can recognise from a job
// location string and resolves each to coordinates. A multi-location listing
// ("Austin, TX, Boston, MA") yields one place per recognised location.
func geoPlaces(loc string) []geoPlace {
	segs := strings.Split(strings.ToLower(loc), ",")
	for i := range segs {
		segs[i] = strings.TrimSpace(segs[i])
	}
	var out []geoPlace
	for i, seg := range segs {
		st := stateAbbrev[seg]
		if st == "" {
			continue
		}
		city := ""
		if i > 0 {
			city = segs[i-1]
		}
		if c, ok := cityCoords[city+", "+st]; ok {
			out = append(out, geoPlace{c[0], c[1], st, geoCity})
		} else if sc, ok := stateCentroids[st]; ok {
			out = append(out, geoPlace{sc[0], sc[1], st, geoState})
		}
	}
	return out
}

// withinSearchArea reports whether a (non-remote) job's location falls inside
// the user's search radius. When there's no usable home location or radius it
// degrades to the plain US check, preserving the previous behavior.
func withinSearchArea(jobLoc string, focus config.JobFocus) bool {
	radius := focus.Location.RadiusMiles
	homes := homePlaces(focus.Location)
	if radius <= 0 || len(homes) == 0 {
		return isUSLocation(jobLoc)
	}
	places := geoPlaces(jobLoc)
	if len(places) == 0 {
		// We couldn't localize the job at all — don't over-drop; defer to the
		// US-or-not check (e.g. a foreign location is still rejected).
		return isUSLocation(jobLoc)
	}
	// Generous buffer on top of the radius: the gazetteer is coarse, so we only
	// drop jobs that are clearly outside the area.
	eff := float64(radius) + math.Max(float64(radius)*0.25, 15)
	for _, h := range homes {
		for _, p := range places {
			if placeWithin(h, p, eff) {
				return true
			}
		}
	}
	return false
}

// placeWithin reports whether place p is within eff miles of home h, using the
// most precise comparison the resolution levels allow.
func placeWithin(h, p geoPlace, eff float64) bool {
	// Both pinned to a city: measure directly (handles same-state-but-far like
	// Houston↔Dallas, and border metros like NYC↔Newark).
	if h.level == geoCity && p.level == geoCity {
		return haversineMiles(h.lat, h.lng, p.lat, p.lng) <= eff
	}
	// One side is only state-precise. Same state → keep (we can't pinpoint the
	// city, so give a local posting the benefit of the doubt).
	if h.state != "" && h.state == p.state {
		return true
	}
	// Different state with coarse coordinates → measure against what we have.
	return haversineMiles(h.lat, h.lng, p.lat, p.lng) <= eff
}
