package predictfun

import (
	"encoding/json"
	"testing"
)

// TestQuoteAcceptsEveryReportedShape pins the shapes the live market list uses
// for bestBid/bestAsk. The venue reports a bare number on most markets, a
// numeric string on some, and a {price,size} object on others, so a plain
// Number field decoded fine in tests and broke on the first real scan.
func TestQuoteAcceptsEveryReportedShape(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		price string
		size  string
	}{
		{name: "number", body: `0.33`, price: "0.33"},
		{name: "string", body: `"0.33"`, price: "0.33"},
		{name: "object", body: `{"price":0.33,"size":123145.4076}`, price: "0.33", size: "123145.4076"},
		{name: "object without size", body: `{"price":"0.07"}`, price: "0.07"},
		{name: "null", body: `null`, price: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var outcome MarketOutcome
			body := `{"name":"Yes","indexSet":1,"bestBid":` + testCase.body + `}`
			if err := json.Unmarshal([]byte(body), &outcome); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got := outcome.BestBid.Price.String(); got != testCase.price {
				t.Fatalf("price = %q, want %q", got, testCase.price)
			}
			if got := outcome.BestBid.Size.String(); got != testCase.size {
				t.Fatalf("size = %q, want %q", got, testCase.size)
			}
		})
	}
}

// TestQuoteRejectsGarbage keeps a broken payload diagnosable instead of being
// read as "no quote". A quoted string is accepted on purpose (Number is text),
// but a JSON array is not a price in any shape the venue reports.
func TestQuoteRejectsGarbage(t *testing.T) {
	var outcome MarketOutcome
	if err := json.Unmarshal([]byte(`{"bestBid":{"price":[1,2],"size":1}}`), &outcome); err == nil {
		t.Fatal("an unparsable price must fail the decode")
	}
}
