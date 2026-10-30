package market

import (
	"regexp"
	"strings"
)

// Category is the coarse market taxonomy used by the opportunity radar. It is
// intentionally smaller than Polymarket's tag graph: the goal is a stable
// filter that is useful for risk decisions, not a mirror of every tag.
type Category string

const (
	CategorySports   Category = "sports"
	CategoryPolitics Category = "politics"
	CategoryCrypto   Category = "crypto"
	CategoryFinance  Category = "finance"
	CategoryTech     Category = "tech"
	CategoryCulture  Category = "culture"
	CategoryOther    Category = "other"
)

func (c Category) Valid() bool {
	switch c {
	case CategorySports, CategoryPolitics, CategoryCrypto, CategoryFinance, CategoryTech, CategoryCulture, CategoryOther:
		return true
	default:
		return false
	}
}

// Classification describes both the display category and the risk flags that
// matter for the 0.95–0.99 tail strategy. SportsGame is true for a single
// match/event, while a season-long futures market (for example an F1 champion)
// is Sports but not a game. OvertimeRisk is set for games because their result
// can change in overtime, extra time, a shootout or a deciding map/set.
type Classification struct {
	Category     Category
	Sports       bool
	SportsGame   bool
	OvertimeRisk bool
}

// categoryPriority keeps the most specific tag when an event carries several
// top-level tags. For example, a crypto tax market tagged both Crypto and
// Politics should appear under Crypto.
var categoryPriority = map[Category]int{
	CategorySports:   0,
	CategoryCrypto:   1,
	CategoryFinance:  2,
	CategoryTech:     3,
	CategoryPolitics: 4,
	CategoryCulture:  5,
	CategoryOther:    6,
}

var categoryBySlug = map[string]Category{
	"sports":   CategorySports,
	"esports":  CategorySports,
	"soccer":   CategorySports,
	"football": CategorySports,
	"nba":      CategorySports,
	"nfl":      CategorySports,
	"mlb":      CategorySports,
	"nhl":      CategorySports,
	"ncaa":     CategorySports,
	"ufc":      CategorySports,
	"mma":      CategorySports,
	"boxing":   CategorySports,
	"tennis":   CategorySports,
	"golf":     CategorySports,
	"cricket":  CategorySports,
	"rugby":    CategorySports,
	"formula1": CategorySports,
	"f1":       CategorySports,

	"politics":                 CategoryPolitics,
	"elections":                CategoryPolitics,
	"geopolitics":              CategoryPolitics,
	"world":                    CategoryPolitics,
	"world-affairs":            CategoryPolitics,
	"international-affairs":    CategoryPolitics,
	"foreign-affairs":          CategoryPolitics,
	"us-politics":              CategoryPolitics,
	"foreign-policy":           CategoryPolitics,
	"midterms":                 CategoryPolitics,
	"midterms-control":         CategoryPolitics,
	"senate-elections":         CategoryPolitics,
	"us-presidential-election": CategoryPolitics,
	"global-elections":         CategoryPolitics,
	"primaries":                CategoryPolitics,

	"crypto":        CategoryCrypto,
	"crypto-prices": CategoryCrypto,
	"bitcoin":       CategoryCrypto,
	"ethereum":      CategoryCrypto,
	"solana":        CategoryCrypto,
	"stablecoins":   CategoryCrypto,
	"airdrops":      CategoryCrypto,

	"finance":         CategoryFinance,
	"business":        CategoryFinance,
	"economy":         CategoryFinance,
	"economic-policy": CategoryFinance,
	"macro-graph":     CategoryFinance,
	"stocks":          CategoryFinance,
	"commodities":     CategoryFinance,
	"ipos":            CategoryFinance,
	"ipo":             CategoryFinance,
	"fed":             CategoryFinance,

	"tech":       CategoryTech,
	"technology": CategoryTech,
	"big-tech":   CategoryTech,
	"science":    CategoryTech,
	"ai":         CategoryTech,
	"space":      CategoryTech,

	"culture":       CategoryCulture,
	"pop-culture":   CategoryCulture,
	"entertainment": CategoryCulture,
	"movies":        CategoryCulture,
	"music":         CategoryCulture,
	"tv":            CategoryCulture,
	"awards":        CategoryCulture,
	"celebrities":   CategoryCulture,
	"celebrity":     CategoryCulture,
}

var (
	// A game start time is the strongest signal, but Gamma sometimes omits it
	// on older or localized payloads. These patterns cover the common match,
	// spread, total and period market titles.
	sportsGamePattern   = regexp.MustCompile(`(?i)(\bvs\.?\b|\bbeat (?:the|by)\b|\bwin on \d{4}-\d{2}-\d{2}\b|^\s*(?:spread|total|moneyline|o/u|over/under)\b|\b(?:halftime|first half|second half|1st half|2nd half|quarter|period|inning|map \d|set \d|game \d)\b|\bmatchup\b|\bexact score\b|\bboth teams to score\b|\bfirst team to score\b|\bteam totals?\b|\bplayer props?\b)`)
	sportsLeaguePattern = regexp.MustCompile(`(?i)\b(nba|nfl|nhl|mlb|ncaa|ufc|mma|f1|formula 1|epl|premier league|champions league|la liga|serie a|bundesliga|ligue 1|soccer|basketball|baseball|hockey|tennis|golf|cricket|rugby|cs2|counter-strike|league of legends|valorant|dota)\b`)
)

// Classify derives a category and the sports risk flags. Event tags are
// authoritative when present; the fallbacks keep the radar useful if the
// optional tag request fails or an upstream response omits tags.
func Classify(m Market, tags []Tag) Classification {
	category := CategoryOther
	for _, tag := range tags {
		candidate, ok := categoryForTag(tag)
		if !ok {
			continue
		}
		if category == CategoryOther || categoryPriority[candidate] < categoryPriority[category] {
			category = candidate
		}
	}

	text := marketText(m)
	if category == CategoryOther && (m.GameStartTime != nil || sportsLeaguePattern.MatchString(text)) {
		category = CategorySports
	}

	sports := category == CategorySports
	game := false
	if m.GameStartTime != nil && !m.GameStartTime.IsZero() {
		// A listed start time means this is a single event, not a season-long
		// futures market. It is a sports signal even if tags are missing.
		category = CategorySports
		sports = true
		game = true
	}
	if sports && !game {
		game = sportsGamePattern.MatchString(text)
	}

	return Classification{
		Category:     category,
		Sports:       sports,
		SportsGame:   game,
		OvertimeRisk: sports && game,
	}
}

func categoryForTag(tag Tag) (Category, bool) {
	if category, ok := categoryBySlug[normalizeTag(tag.Slug)]; ok {
		return category, true
	}
	label := normalizeTag(tag.Label)
	if category, ok := categoryBySlug[label]; ok {
		return category, true
	}
	switch {
	case strings.Contains(label, "sport"):
		return CategorySports, true
	case strings.Contains(label, "politic") || strings.Contains(label, "election"):
		return CategoryPolitics, true
	case strings.Contains(label, "crypto") || strings.Contains(label, "bitcoin"):
		return CategoryCrypto, true
	case strings.Contains(label, "financ") || strings.Contains(label, "business") || strings.Contains(label, "econom"):
		return CategoryFinance, true
	case strings.Contains(label, "tech") || strings.Contains(label, "science") || label == "ai":
		return CategoryTech, true
	case strings.Contains(label, "culture") || strings.Contains(label, "entertain") ||
		strings.Contains(label, "movie") || strings.Contains(label, "music") || strings.Contains(label, "celebrit"):
		return CategoryCulture, true
	default:
		return "", false
	}
}

func normalizeTag(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func marketText(m Market) string {
	return strings.ToLower(strings.Join([]string{m.Question, m.GroupItemTitle, m.EventTitle, m.Slug}, " "))
}
