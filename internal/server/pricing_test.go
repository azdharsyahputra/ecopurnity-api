package server

import (
	"reflect"
	"testing"
)

func TestSuggestPrice(t *testing.T) {
	if suggestPrice([]float64{1000, 2000}) != nil {
		t.Fatal("2 samples must give nothing")
	}
	got := suggestPrice([]float64{1000, 2000, 3000, 4000, 5000, 0})
	want := &priceSuggestion{Median: 3000, Low: 2000, High: 4000, Sample: 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if g := suggestPrice([]float64{82000, 82975, 88000}); g.Median != 83000 {
		t.Fatalf("rounding to Rp 50: %+v", g)
	}
	if w := itemWords("Biji kopi arabika, grade-1"); !reflect.DeepEqual(w, []string{"biji", "kopi", "arabika", "grade"}) {
		t.Fatalf("words %v", w)
	}
	if !sharesWord("Kopi Arabika Garut Q4", []string{"arabika"}) || sharesWord("Pupuk organik", []string{"kopi"}) {
		t.Fatal("sharesWord")
	}
}
