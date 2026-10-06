// Package sysconfig reads a trading system's config.json and adapts the three kinds the framework produces —
// MT5, IB and crypto — to the one view the Supervisor needs: every logical system as its runner LOGS it
// (okmich_quant_core LogicalSystemIdentity = strategy / symbol / timeframe in minutes, LOGGING_CONTRACT §6). The
// importer and discovery both classify through here, so the live tree, the log tree and status.json agree for
// every broker.
//
// Detection, by markers only the kind's own runner reads:
//
//	crypto  a top-level "venue" object, or a sleeve with "market_symbol"  (okmich_quant_crypto CryptoSystemConfig)
//	ib      a top-level "ib_contracts" map                                 (the lab's IB run.py convention)
//	mt5     neither
//
// Each kind writes its timeframe differently, and each runner converts it to minutes before logging; the tables
// below mirror those conversions exactly (okmich_quant_mt5 / okmich_quant_ib / okmich_quant_crypto timeframe_utils).
package sysconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind is the broker family a config.json was written for.
type Kind string

const (
	KindMT5    Kind = "mt5"
	KindIB     Kind = "ib"
	KindCrypto Kind = "crypto"
)

// Sleeve is one logical system as its runner logs it.
type Sleeve struct {
	Name             string // strategy code
	Symbol           string // the logged symbol (crypto: market_symbol with ':' -> '-', as derive_log_symbol)
	TimeframeMinutes int    // 0 after Classify; set by Parse
}

// Config is a classified config.json.
type Config struct {
	Kind    Kind
	Name    string   // the top-level system name
	Multi   bool     // a non-empty strategies[] (LOGGING_CONTRACT §7.1: runner root <strategy>-multi)
	Sleeves []Sleeve // every strategies[] entry, or the single `strategy`
}

// ErrUnclassified: the config has neither a `strategy` object nor a non-empty `strategies[]`.
var ErrUnclassified = errors.New("config.json classifies as neither single (a `strategy` object) nor multi " +
	"(a non-empty `strategies[]`)")

type rawSleeve struct {
	Name         string          `json:"name"`
	Symbol       string          `json:"symbol"`
	MarketSymbol string          `json:"market_symbol"`
	Timeframe    json.RawMessage `json:"timeframe"`
}

type rawConfig struct {
	Name        string          `json:"name"`
	Strategy    *rawSleeve      `json:"strategy"`
	Strategies  []rawSleeve     `json:"strategies"`
	Venue       json.RawMessage `json:"venue"`
	IBContracts json.RawMessage `json:"ib_contracts"`
}

// Classify reads the kind, the single/multi shape and each sleeve's logged symbol WITHOUT interpreting timeframes.
// Discovery uses it, so a deployed system is never mis-classified (and dropped from its runner root) only because
// the Supervisor cannot read its timeframe.
func Classify(raw []byte) (Config, error) {
	c, _, err := classify(raw)
	return c, err
}

// Parse is Classify plus every sleeve's timeframe in minutes, converted by the kind's own rules. A sleeve whose
// timeframe its runner could not label is an error naming the sleeve. The importer uses it.
func Parse(raw []byte) (Config, error) {
	c, sleeves, err := classify(raw)
	if err != nil {
		return Config{}, err
	}
	for i := range c.Sleeves {
		minutes, err := TimeframeMinutes(c.Kind, sleeves[i].Timeframe)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", sleeveLabel(c.Multi, i), err)
		}
		c.Sleeves[i].TimeframeMinutes = minutes
	}
	return c, nil
}

func classify(raw []byte) (Config, []rawSleeve, error) {
	var rc rawConfig
	if err := json.Unmarshal(raw, &rc); err != nil {
		return Config{}, nil, fmt.Errorf("config.json is not valid JSON: %w", err)
	}
	sleeves, multi := rc.Strategies, true
	if len(sleeves) == 0 {
		if rc.Strategy == nil {
			return Config{}, nil, ErrUnclassified
		}
		sleeves, multi = []rawSleeve{*rc.Strategy}, false
	}
	kind, err := detectKind(rc, sleeves)
	if err != nil {
		return Config{}, nil, err
	}
	c := Config{Kind: kind, Name: rc.Name, Multi: multi}
	for _, s := range sleeves {
		c.Sleeves = append(c.Sleeves, Sleeve{Name: s.Name, Symbol: loggedSymbol(kind, s)})
	}
	return c, sleeves, nil
}

func detectKind(rc rawConfig, sleeves []rawSleeve) (Kind, error) {
	crypto := present(rc.Venue)
	for _, s := range sleeves {
		crypto = crypto || s.MarketSymbol != ""
	}
	ib := present(rc.IBContracts)
	switch {
	case crypto && ib:
		return "", errors.New("config.json carries both crypto (venue / market_symbol) and IB (ib_contracts) " +
			"markers — which runner is it for?")
	case crypto:
		return KindCrypto, nil
	case ib:
		return KindIB, nil
	}
	return KindMT5, nil
}

func present(m json.RawMessage) bool {
	s := strings.TrimSpace(string(m))
	return s != "" && s != "null"
}

// loggedSymbol is the symbol the runner's LogicalSystemIdentity carries.
func loggedSymbol(kind Kind, s rawSleeve) string {
	if kind == KindCrypto {
		return strings.ReplaceAll(s.MarketSymbol, ":", "-") // okmich_quant_crypto.config.derive_log_symbol
	}
	return s.Symbol
}

func sleeveLabel(multi bool, i int) string {
	if multi {
		return fmt.Sprintf("strategies[%d].timeframe", i)
	}
	return "strategy.timeframe"
}

// TimeframeMinutes converts a config timeframe to the minutes its runner logs (the path's <timeframe> segment).
func TimeframeMinutes(kind Kind, raw json.RawMessage) (int, error) {
	switch kind {
	case KindMT5:
		var c int
		if err := json.Unmarshal(raw, &c); err != nil {
			return 0, fmt.Errorf("MT5 timeframe must be a TIMEFRAME_* constant (an int, e.g. 5 = M5, 16385 = H1), "+
				"got %s", strings.TrimSpace(string(raw)))
		}
		if m, ok := mt5Minutes[c]; ok {
			return m, nil
		}
		return 0, fmt.Errorf("unknown MT5 timeframe constant %d", c)
	case KindIB:
		var bar string
		if err := json.Unmarshal(raw, &bar); err != nil {
			return 0, fmt.Errorf("IB timeframe must be a bar size such as \"5 mins\" (the IB runner rejects %s)",
				strings.TrimSpace(string(raw)))
		}
		if m, ok := ibMinutes[bar]; ok {
			return m, nil
		}
		return 0, fmt.Errorf("unsupported IB bar size %q", bar)
	case KindCrypto:
		var tf string
		if err := json.Unmarshal(raw, &tf); err != nil {
			return 0, fmt.Errorf("crypto timeframe must be a CCXT timeframe such as \"15m\", got %s",
				strings.TrimSpace(string(raw)))
		}
		return ccxtMinutes(tf)
	}
	return 0, fmt.Errorf("unknown config kind %q", kind)
}

// mt5Minutes mirrors okmich_quant_mt5.timeframe_utils.timeframe_minutes_dict (MetaTrader5 TIMEFRAME_* constants).
var mt5Minutes = map[int]int{
	1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 10: 10, 12: 12, 15: 15, 20: 20, 30: 30, // M1 .. M30
	16385: 60, 16386: 120, 16387: 180, 16388: 240, 16390: 360, 16392: 480, 16396: 720, // H1 .. H12
	16408: 1440, 32769: 10080, 49153: 43200, // D1, W1, MN1
}

// ibMinutes mirrors okmich_quant_ib.timeframe_utils.BAR_SIZE_MINUTES.
var ibMinutes = map[string]int{
	"1 min": 1, "2 mins": 2, "5 mins": 5, "10 mins": 10, "15 mins": 15, "30 mins": 30,
	"1 hour": 60, "2 hours": 120, "4 hours": 240, "1 day": 1440,
}

// ccxtTimeframe / ccxtUnitSeconds mirror ccxt Exchange.parse_timeframe.
var (
	ccxtTimeframe   = regexp.MustCompile(`^([1-9][0-9]{0,5})([smhdwMy])$`)
	ccxtUnitSeconds = map[string]int{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800, "M": 2592000, "y": 31536000}
)

// maxCryptoMinutes mirrors okmich_quant_crypto.timeframe_utils.MAX_TIMEFRAME_MINUTES.
const maxCryptoMinutes = 1440

// ccxtMinutes mirrors okmich_quant_crypto.timeframe_utils.timeframe_to_minutes: whole minutes, 1m .. 1d, and
// dividing a day evenly (so candles stay on core's UTC epoch-minute grid).
func ccxtMinutes(tf string) (int, error) {
	m := ccxtTimeframe.FindStringSubmatch(tf)
	if m == nil {
		return 0, fmt.Errorf("unparseable CCXT timeframe %q", tf)
	}
	n, _ := strconv.Atoi(m[1])
	seconds := n * ccxtUnitSeconds[m[2]]
	if seconds%60 != 0 {
		return 0, fmt.Errorf("timeframe %q is not a whole number of minutes", tf)
	}
	minutes := seconds / 60
	if minutes > maxCryptoMinutes {
		return 0, fmt.Errorf("timeframe %q is outside 1m..1d", tf)
	}
	if maxCryptoMinutes%minutes != 0 {
		return 0, fmt.Errorf("timeframe %q does not divide a day evenly", tf)
	}
	return minutes, nil
}
