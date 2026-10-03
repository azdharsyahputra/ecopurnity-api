package mail

import (
	"strings"
	"testing"
	"time"
)

func TestTemplates(t *testing.T) {
	v := VerifyCode("a@b.id", "Rina <script>", "042137", 10*time.Minute)
	if v.Subject != "042137 adalah kode verifikasi Ecopurnity kamu" || v.Code != "042137" {
		t.Fatalf("subject %q", v.Subject)
	}
	for _, want := range []string{"042137", "10 menit", "Rina &lt;script&gt;", "<!doctype html>", "Jangan bagikan"} {
		if !strings.Contains(v.HTML, want) {
			t.Errorf("verify html lacks %q", want)
		}
	}
	if strings.Contains(v.HTML, "<script>") || !strings.Contains(v.Body, "042137") || strings.Contains(v.Body, "<") && !strings.Contains(v.Body, "Rina <script>") {
		t.Errorf("escaping / text part wrong")
	}

	r := ResetPassword("a@b.id", "Rina", "https://app.example/reset-password?token=x&y=1", time.Hour)
	if r.Link == "" || !strings.Contains(r.HTML, "token=x&amp;y=1") || !strings.Contains(r.Body, "token=x&y=1") || !strings.Contains(r.HTML, "60 menit") {
		t.Errorf("reset: %q\n%s", r.Body, r.HTML)
	}

	n := NotificationEmail("a@b.id", "Rina", Notification{Type: "outbid", Title: "Kamu tersalip di Kopi Q4", Body: "harga terbaik sekarang Rp 82.000/kg.",
		URL: "https://app.example/auctions/1"}, "https://app.example/app/settings")
	if n.Subject != "Kamu tersalip di Kopi Q4" || !strings.Contains(n.HTML, "Tersalip") || !strings.Contains(n.HTML, "Lihat detail") ||
		!strings.Contains(n.HTML, "Atur notifikasi email") || !strings.Contains(n.Body, "https://app.example/auctions/1") {
		t.Errorf("notification: %s\n%s", n.Subject, n.HTML)
	}
}
