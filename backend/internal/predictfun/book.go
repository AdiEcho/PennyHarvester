package predictfun

import (
	"math"
	"math/big"
	"sort"
	"strconv"

	"pennyharvester/backend/internal/venue"
)

// Outcome is one side of a binary Predict.fun market. The venue's orderbook
// endpoint only publishes the Yes side, so OutcomeNo is always derived.
type Outcome string

const (
	// OutcomeYes is the native side the venue publishes an orderbook for.
	OutcomeYes Outcome = "Yes"
	// OutcomeNo is derived as 1-p with its depth mirrored; the venue has no No
	// orderbook of its own.
	OutcomeNo Outcome = "No"
)

// PriceLevel is one decoded, tick-aligned book level.
type PriceLevel struct {
	Price float64
	Size  float64
}

// TranslatedBook is a two-sided view of one outcome, in the direction a taker
// would trade it: Asks is what a buyer pays, Bids is what a seller receives.
type TranslatedBook struct {
	Outcome  Outcome
	MarketID string
	// Bids is sorted by descending price, Asks by ascending price.
	Bids []PriceLevel
	Asks []PriceLevel
}

// BestAsk returns the lowest ask, i.e. the price a market buy pays.
func (b TranslatedBook) BestAsk() (float64, bool) {
	if len(b.Asks) == 0 {
		return 0, false
	}
	return b.Asks[0].Price, true
}

// BestBid returns the highest bid, i.e. the price a market sell receives.
func (b TranslatedBook) BestBid() (float64, bool) {
	if len(b.Bids) == 0 {
		return 0, false
	}
	return b.Bids[0].Price, true
}

// YesBestAsk returns the lowest ask on the native Yes side.
func (b Book) YesBestAsk() (float64, bool) {
	return BookForOutcome(b, OutcomeYes, 0).BestAsk()
}

// YesBestBid returns the highest bid on the native Yes side.
func (b Book) YesBestBid() (float64, bool) {
	return BookForOutcome(b, OutcomeYes, 0).BestBid()
}

// BookForOutcome translates the Yes-only orderbook into the requested side.
//
// Yes passes through unchanged. No is the complement: every Yes price p becomes
// 1-p, and because a price and its complement invert the direction of the book,
// Yes bids become No asks and Yes asks become No bids. The user buys No at
// 1-(best Yes bid), which is exactly "sell Yes to someone willing to buy it" —
// the only way the single published side can express a No trade.
//
// Derived prices are quantized DOWN to the tick. Rounding down keeps the price a
// valid multiple of the tick and never asks the taker to accept a worse price
// than the one that was displayed.
//
// A mid computed from this book is indicative only: both sides come from one
// Yes book, so the No mid is arithmetically 1-yesMid, not an independently
// discovered No quote.
func BookForOutcome(book Book, outcome Outcome, tick float64) TranslatedBook {
	decimals := priceDecimalsFor(tick)
	translated := TranslatedBook{Outcome: outcome, MarketID: book.MarketID.String()}

	if outcome != OutcomeNo {
		for _, level := range book.Asks {
			if price, size, ok := decodeLevel(level, decimals); ok {
				translated.Asks = append(translated.Asks, PriceLevel{Price: price, Size: size})
			}
		}
		for _, level := range book.Bids {
			if price, size, ok := decodeLevel(level, decimals); ok {
				translated.Bids = append(translated.Bids, PriceLevel{Price: price, Size: size})
			}
		}
	} else {
		for _, level := range book.Bids {
			if price, size, ok := decodeLevel(level, decimals); ok {
				if inverted := complement(price, tick); inverted > 0 {
					translated.Asks = append(translated.Asks, PriceLevel{Price: inverted, Size: size})
				}
			}
		}
		for _, level := range book.Asks {
			if price, size, ok := decodeLevel(level, decimals); ok {
				if inverted := complement(price, tick); inverted > 0 {
					translated.Bids = append(translated.Bids, PriceLevel{Price: inverted, Size: size})
				}
			}
		}
	}

	sort.Slice(translated.Asks, func(i, j int) bool { return translated.Asks[i].Price < translated.Asks[j].Price })
	sort.Slice(translated.Bids, func(i, j int) bool { return translated.Bids[i].Price > translated.Bids[j].Price })
	return translated
}

// complement maps a Yes price onto the No price and quantizes it down.
func complement(price, tick float64) float64 {
	value := quantizeDown(1-price, tick)
	if value <= 0 || value >= 1 {
		return 0
	}
	return value
}

func decodeLevel(level Level, decimals int) (price, size float64, ok bool) {
	price = parsePrice(level.Price, decimals)
	size = parseSize(level.Size)
	if price <= 0 || price >= 1 || size <= 0 {
		return 0, 0, false
	}
	return price, size, true
}

// parsePrice decodes a price that must land in (0,1). The API sends prices as
// decimals ("0.42"); the two fallbacks exist because the same payload family
// also carries 18-decimal fixed-point numbers elsewhere, and a price scaled by
// the market's tick precision is a plausible third encoding. Anything that
// cannot be decoded returns 0, which every caller treats as "no such level"
// rather than as a price of zero.
func parsePrice(value Number, decimals int) float64 {
	raw, err := value.Float64()
	if err != nil || raw <= 0 || math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0
	}
	if raw < 1 {
		return raw
	}
	if raw >= 1e15 {
		if scaled := raw / 1e18; scaled < 1 {
			return scaled
		}
	}
	if decimals > 0 {
		if scaled := raw / math.Pow10(decimals); scaled < 1 {
			return scaled
		}
	}
	return 0
}

// parseSize decodes a book size into shares. Shares are 18-decimal integers on
// this venue, so a value with 15 or more digits is scaled down. Smaller values
// are read as plain share counts: no real book holds 1e15 shares, and treating a
// plain "12.5" as wei would understate depth by 18 orders of magnitude.
//
// The heuristic never reaches a signature: order amounts are computed from the
// caller's USDT amount and the tick-quantized limit price, not from book sizes.
func parseSize(value Number) float64 {
	raw, err := value.Float64()
	if err != nil || raw <= 0 || math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0
	}
	if raw >= 1e15 {
		return raw / 1e18
	}
	return raw
}

// unscale18 converts an 18-decimal base unit amount into a decimal value.
// venue.Unscale goes through big.Rat, so 1e18 becomes exactly 1.0.
func unscale18(raw *big.Int) float64 {
	return venue.Unscale(raw, venue.USDTDecimals)
}

// quantizeDown rounds a price down to a whole tick and strips the binary
// floating point noise afterwards. Without the second step a complement such as
// 1-0.31 becomes 0.6900000000000001 and the signed order carries a price that is
// only incidentally on the tick.
func quantizeDown(price, tick float64) float64 {
	if tick <= 0 {
		return price
	}
	return roundToTickDecimals(venue.Quantize(price, tick), tick)
}

// roundToTickDecimals snaps a tick-aligned price to the tick's decimal places.
func roundToTickDecimals(price, tick float64) float64 {
	if tick <= 0 {
		return price
	}
	scale := math.Pow10(venue.PriceDecimalsForTick(tick))
	return math.Round(price*scale) / scale
}

// priceDecimalsFor reports how many decimals a tick needs, or zero when the
// tick is unknown and prices must be read as-is.
func priceDecimalsFor(tick float64) int {
	if tick <= 0 {
		return 0
	}
	return venue.PriceDecimalsForTick(tick)
}

// formatPrice renders a price for the API's decimal-string fields.
func formatPrice(price float64) string {
	return strconv.FormatFloat(price, 'f', -1, 64)
}
