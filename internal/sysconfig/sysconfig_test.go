package sysconfig

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Shapes copied from real configs: the lab's systems/ctl (MT5), the IB package tests ("5 mins"), and
// systems/crypto_rsi_demo (crypto).
const (
	mt5Multi = `{"name":"CtlPullback","runloop":{"sleep_interval":0.5},"strategies":[
		{"name":"ctl_pullback","symbol":"EURUSD.r","timeframe":5,"magic":1},
		{"name":"ctl_pullback","symbol":"Volatility 75 Index","timeframe":5,"magic":2}]}`
	mt5SingleH1 = `{"name":"Idx","runloop":{},"strategy":{"name":"idxmon","symbol":"US500.r","timeframe":16385},"strategies":[]}`
	ibSingle    = `{"name":"Rsi2","runloop":{},"strategy":{"name":"rsi2_mean_reversion","symbol":"SPY","timeframe":"5 mins"},
		"strategies":[],"ib_contracts":{"rsi2_mean_reversion":{"sec_type":"STK"}}}`
	cryptoMulti = `{"name":"crypto_rsi_demo_bybit","venue":{"exchange_id":"bybit","environment":"demo"},"strategies":[
		{"name":"crypto_rsi_bybit","market_symbol":"BTC/USDT:USDT","market_type":"linear_perp","timeframe":"15m"},
		{"name":"crypto_rsi_bybit","market_symbol":"ETH/USDT:USDT","market_type":"linear_perp","timeframe":"15m"}]}`
)

func mustParse(t *testing.T, raw string) Config {
	t.Helper()
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDetectsAndAdaptsEachKind(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		kind    Kind
		multi   bool
		sleeves []Sleeve
	}{
		{"mt5 multi", mt5Multi, KindMT5, true, []Sleeve{
			{"ctl_pullback", "EURUSD.r", 5}, {"ctl_pullback", "Volatility 75 Index", 5}}},
		// H1 is MT5 constant 16385, but the runner logs (and paths use) minutes.
		{"mt5 single H1", mt5SingleH1, KindMT5, false, []Sleeve{{"idxmon", "US500.r", 60}}},
		{"ib single", ibSingle, KindIB, false, []Sleeve{{"rsi2_mean_reversion", "SPY", 5}}},
		// Crypto logs market_symbol with ':' -> '-' (derive_log_symbol) and the CCXT timeframe in minutes.
		{"crypto multi", cryptoMulti, KindCrypto, true, []Sleeve{
			{"crypto_rsi_bybit", "BTC/USDT-USDT", 15}, {"crypto_rsi_bybit", "ETH/USDT-USDT", 15}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.raw)
			if c.Kind != tc.kind || c.Multi != tc.multi {
				t.Fatalf("kind=%s multi=%v, want %s %v", c.Kind, c.Multi, tc.kind, tc.multi)
			}
			if len(c.Sleeves) != len(tc.sleeves) {
				t.Fatalf("sleeves = %+v", c.Sleeves)
			}
			for i, want := range tc.sleeves {
				if c.Sleeves[i] != want {
					t.Errorf("sleeve %d = %+v, want %+v", i, c.Sleeves[i], want)
				}
			}
		})
	}
}

func TestCryptoDetectedByMarketSymbolAlone(t *testing.T) {
	c := mustParse(t, `{"name":"x","strategy":{"name":"s","market_symbol":"ETH/USDT","timeframe":"1h"}}`)
	if c.Kind != KindCrypto || c.Sleeves[0] != (Sleeve{"s", "ETH/USDT", 60}) {
		t.Fatalf("got %+v", c)
	}
}

func TestTimeframeConversions(t *testing.T) {
	ok := []struct {
		kind Kind
		raw  string
		want int
	}{
		{KindMT5, `1`, 1}, {KindMT5, `30`, 30}, {KindMT5, `16388`, 240}, {KindMT5, `16408`, 1440},
		{KindMT5, `32769`, 10080}, {KindMT5, `49153`, 43200},
		{KindIB, `"1 min"`, 1}, {KindIB, `"30 mins"`, 30}, {KindIB, `"1 hour"`, 60}, {KindIB, `"1 day"`, 1440},
		{KindCrypto, `"1m"`, 1}, {KindCrypto, `"5m"`, 5}, {KindCrypto, `"4h"`, 240}, {KindCrypto, `"1d"`, 1440},
		{KindCrypto, `"60s"`, 1}, {KindCrypto, `"720m"`, 720},
	}
	for _, tc := range ok {
		got, err := TimeframeMinutes(tc.kind, json.RawMessage(tc.raw))
		if err != nil || got != tc.want {
			t.Errorf("%s %s = %d, %v; want %d", tc.kind, tc.raw, got, err, tc.want)
		}
	}
	bad := []struct {
		kind Kind
		raw  string
		msg  string
	}{
		{KindMT5, `7`, "unknown MT5 timeframe constant 7"},
		{KindMT5, `0`, "unknown MT5 timeframe constant 0"},
		{KindMT5, `"5m"`, "TIMEFRAME_* constant"},
		{KindIB, `5`, "bar size"}, // the old-format IB configs: the IB runner itself rejects an int
		{KindIB, `"3 mins"`, "unsupported IB bar size"},
		{KindCrypto, `15`, "CCXT timeframe"},
		{KindCrypto, `"1w"`, "outside 1m..1d"},
		{KindCrypto, `"7m"`, "divide a day"},
		{KindCrypto, `"30s"`, "whole number of minutes"},
		{KindCrypto, `"15"`, "unparseable"},
		{KindCrypto, `"0m"`, "unparseable"},
	}
	for _, tc := range bad {
		if _, err := TimeframeMinutes(tc.kind, json.RawMessage(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s %s: err = %v, want it to mention %q", tc.kind, tc.raw, err, tc.msg)
		}
	}
}

func TestClassifyIgnoresTimeframesParseDoesNot(t *testing.T) {
	raw := strings.Replace(cryptoMulti, `"timeframe":"15m"}]`, `"timeframe":"1w"}]`, 1)
	c, err := Classify([]byte(raw))
	if err != nil || !c.Multi || c.Kind != KindCrypto || c.Sleeves[1].Symbol != "ETH/USDT-USDT" {
		t.Fatalf("Classify = %+v, %v", c, err)
	}
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "strategies[1].timeframe") {
		t.Fatalf("Parse err = %v, want it to name strategies[1].timeframe", err)
	}
}

func TestRejections(t *testing.T) {
	if _, err := Parse([]byte(`{not json`)); err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("bad json: %v", err)
	}
	if _, err := Parse([]byte(`{"name":"x","strategies":[]}`)); !errors.Is(err, ErrUnclassified) {
		t.Errorf("unclassified: %v", err)
	}
	both := `{"venue":{"exchange_id":"bybit"},"ib_contracts":{},"strategy":{"name":"s","symbol":"SPY","timeframe":"5 mins"}}`
	if _, err := Parse([]byte(both)); err == nil || !strings.Contains(err.Error(), "both crypto") {
		t.Errorf("both markers: %v", err)
	}
	legacyIB := strings.Replace(ibSingle, `"timeframe":"5 mins"`, `"timeframe":5`, 1)
	if _, err := Parse([]byte(legacyIB)); err == nil || !strings.Contains(err.Error(), "strategy.timeframe") {
		t.Errorf("legacy IB int timeframe: %v", err)
	}
}
