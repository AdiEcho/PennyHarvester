package market

import (
	"testing"
	"time"
)

func TestClassifySportsGame(t *testing.T) {
	start := time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC)
	item := Market{
		Question:      "Will the Mavericks beat the Grizzlies by more than 5.5 points in their December 4 matchup?",
		EventTitle:    "Mavericks vs. Grizzlies",
		GameStartTime: &start,
	}
	got := Classify(item, []Tag{{Slug: "sports"}, {Slug: "nba"}})
	if got.Category != CategorySports || !got.Sports || !got.SportsGame || !got.OvertimeRisk {
		t.Fatalf("expected a sports game with overtime risk, got %+v", got)
	}
}

func TestClassifySportsFuturesIsNotGame(t *testing.T) {
	item := Market{
		Question: "Will Lando Norris be the 2026 F1 Drivers' Champion?",
		Slug:     "will-lando-norris-be-the-2026-f1-drivers-champion",
	}
	got := Classify(item, []Tag{{Slug: "sports"}, {Slug: "formula1"}})
	if got.Category != CategorySports || !got.Sports {
		t.Fatalf("expected a sports futures market, got %+v", got)
	}
	if got.SportsGame || got.OvertimeRisk {
		t.Fatalf("season futures must not be treated as a game, got %+v", got)
	}
}

func TestClassifyPoliticsAndCryptoPriority(t *testing.T) {
	politics := Classify(Market{Question: "Will the candidate win the election?"}, []Tag{{Slug: "politics"}, {Slug: "elections"}})
	if politics.Category != CategoryPolitics || politics.SportsGame {
		t.Fatalf("unexpected politics classification: %+v", politics)
	}

	crypto := Classify(Market{Question: "Will Trump eliminate capital gains tax on crypto?"}, []Tag{{Slug: "politics"}, {Slug: "crypto"}})
	if crypto.Category != CategoryCrypto {
		t.Fatalf("crypto is more specific than politics, got %+v", crypto)
	}
}

func TestClassifyFallsBackToGameStartTime(t *testing.T) {
	start := time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)
	got := Classify(Market{Question: "Will the home team win?", GameStartTime: &start}, nil)
	if got.Category != CategorySports || !got.SportsGame || !got.OvertimeRisk {
		t.Fatalf("a game start time must identify a sports game, got %+v", got)
	}
}

func TestClassifyDetectsGameTitleWithoutStartTime(t *testing.T) {
	got := Classify(Market{Question: "Spread: CA San Isidro (-8.5)", Slug: "spread-ca-san-isidro-8pt5"}, []Tag{{Slug: "sports"}})
	if !got.SportsGame || !got.OvertimeRisk {
		t.Fatalf("spread markets must be treated as games, got %+v", got)
	}
}
