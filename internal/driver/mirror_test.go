package driver

import "testing"

func TestMirrorGrammar(t *testing.T) {
	for _, s := range []string{"[cg-mirror:viva:12]", " [cg-mirror:lunda:Ab_C-12] note", "[cg-mirror:yclients:y-12]", "[cg-mirror:fitness1c:x-1]"} {
		if ClassifyNote(s, true, true).Class != MirrorBooking {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"[cg-mirror:viva:12", "[cg-mirror:Viva:12]", "[cg-mirror:viva:1 2]", "[cg-mirror:viva:12]!", "[cg-mirror:other:12]", "[cg-mirror:viva:12:3]"} {
		if ClassifyNote(s, true, true).Class != DamagedMirror {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"[cg:booker]", "note [cg-mirror:viva:12]"} {
		if ClassifyNote(s, true, true).Class == MirrorBooking {
			t.Fatal(s)
		}
	}
	a := ClassifyNote("[cg-mirror:viva:12]", true, true)
	b := ClassifyNote("[cg-mirror:viva:123]", true, true)
	c := ClassifyNote("[cg-mirror:lunda:Ab]", true, true)
	d := ClassifyNote("[cg-mirror:lunda:ab]", true, true)
	if a.SourceID == b.SourceID || c.SourceID == d.SourceID {
		t.Fatal("ids must compare exactly")
	}
	if ClassifyNote("[cg-mirror:viva:12]", false, false).Class != ClubBooking {
		t.Fatal("a comment alone is not ownership")
	}
	p := ClassifyNote("[cg:booker] [cg-pair:e-123] private", false, true)
	if p.Class != PartnerBooking || p.PairID != "e-123" || p.Partner != "booker" {
		t.Fatalf("%+v", p)
	}
}
