package server

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

type priceSuggestion struct {
	Median, Low, High int
	Sample            int
}

func quantile(sorted []float64, q float64) float64 {
	pos := float64(len(sorted)-1) * q
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

func suggestPrice(samples []float64) *priceSuggestion {
	s := make([]float64, 0, len(samples))
	for _, x := range samples {
		if x > 0 {
			s = append(s, x)
		}
	}
	if len(s) < 3 {
		return nil
	}
	sort.Float64s(s)
	round := func(x float64) int { return int(math.Round(x/50) * 50) }
	return &priceSuggestion{Median: round(quantile(s, 0.5)), Low: round(quantile(s, 0.25)), High: round(quantile(s, 0.75)), Sample: len(s)}
}

func itemWords(item string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(item), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 4 {
			out = append(out, w)
		}
	}
	return out
}

func sharesWord(text string, words []string) bool {
	t := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}
