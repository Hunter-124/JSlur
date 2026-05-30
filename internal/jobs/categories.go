package jobs

import (
	"sort"
	"strings"
)

// museCategories is The Muse's category taxonomy. The Muse has no categories
// endpoint, so we ship its known list; the API filters by the category name, so
// ID == Label here.
var museCategories = []Category{
	{"Software Engineering", "Software Engineering"},
	{"Data Science", "Data Science"},
	{"Data and Analytics", "Data and Analytics"},
	{"IT", "IT"},
	{"Science and Engineering", "Science and Engineering"},
	{"Healthcare", "Healthcare"},
	{"Mental Health", "Mental Health"},
	{"Accounting and Finance", "Accounting and Finance"},
	{"Sales", "Sales"},
	{"Marketing", "Marketing"},
	{"Design and UX", "Design and UX"},
	{"Human Resources and Recruitment", "Human Resources and Recruitment"},
	{"Project Management", "Project Management"},
	{"Product Management", "Product Management"},
	{"Education", "Education"},
	{"Legal", "Legal"},
	{"Customer Service", "Customer Service"},
	{"Business Operations", "Business Operations"},
	{"Administration and Office", "Administration and Office"},
	{"Construction", "Construction"},
	{"Manufacturing and Warehouse", "Manufacturing and Warehouse"},
	{"Retail", "Retail"},
	{"Restaurant and Food Service", "Restaurant and Food Service"},
	{"Transportation and Logistics", "Transportation and Logistics"},
}

// interestHints maps common role words to category-label fragments so the
// no-AI heuristic can still bridge e.g. "nurse" -> "Healthcare". The AI selector
// does this far better; this only powers the fallback.
var interestHints = map[string][]string{
	"nurse": {"healthcare"}, "rn ": {"healthcare"}, "clinical": {"healthcare"},
	"medical": {"healthcare"}, "patient": {"healthcare"}, "pharmac": {"healthcare"},
	"physician": {"healthcare"}, "therapist": {"healthcare", "mental health"},
	"engineer": {"science and engineering", "software"}, "mechanical": {"science and engineering"},
	"electrical": {"science and engineering"}, "chemist": {"science and engineering"},
	"scientist": {"science and engineering", "data"}, "laboratory": {"science and engineering"},
	"software": {"software"}, "developer": {"software"}, "programmer": {"software"},
	"account": {"accounting and finance"}, "finance": {"accounting and finance"},
	"audit": {"accounting and finance"}, "bookkeep": {"accounting and finance"},
	"teacher": {"education"}, "professor": {"education"}, "tutor": {"education"},
	"lawyer": {"legal"}, "attorney": {"legal"}, "paralegal": {"legal"},
	"sales": {"sales"}, "designer": {"design"}, "marketing": {"marketing"},
	"recruit": {"human resources"}, "data": {"data"}, "product manager": {"product management"},
	"warehouse": {"manufacturing"}, "driver": {"transportation"}, "retail": {"retail"},
	"construction": {"construction"}, "support": {"customer service"},
}

// HeuristicSelect ranks categories by overlap between the interest text (plus
// any hint fragments it triggers) and each category label, returning up to max
// matches. Used as the fallback when no AI provider is available.
func HeuristicSelect(interest string, available []Category, max int) []Category {
	il := strings.ToLower(interest)
	frags := map[string]bool{}
	for _, w := range strings.FieldsFunc(il, func(r rune) bool { return !(r >= 'a' && r <= 'z') }) {
		if len(w) > 2 {
			frags[w] = true
		}
	}
	for trigger, cats := range interestHints {
		if strings.Contains(il, strings.TrimSpace(trigger)) {
			for _, c := range cats {
				frags[c] = true
			}
		}
	}

	type scored struct {
		c Category
		n int
	}
	var ranked []scored
	for _, c := range available {
		label := strings.ToLower(c.Label)
		n := 0
		for f := range frags {
			if strings.Contains(label, f) {
				n++
			}
		}
		if n > 0 {
			ranked = append(ranked, scored{c, n})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].n > ranked[j].n })

	out := make([]Category, 0, max)
	for _, s := range ranked {
		if len(out) >= max {
			break
		}
		out = append(out, s.c)
	}
	return out
}
