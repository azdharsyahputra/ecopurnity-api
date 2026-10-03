// Command mailpreview sends one sample of every email template through the configured SMTP server (locally: Mailpit,
// read them at http://localhost:8025). Use it to check how the templates look before pointing SMTP at a real provider.
//
//	make mail-preview [TO=you@example.com]
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/azdharsyahputra/ecopurnity-api/internal/mail"
)

func main() {
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		fmt.Fprintln(os.Stderr, "SMTP_HOST is empty (see .env)")
		os.Exit(1)
	}
	port, _ := strconv.Atoi(os.Getenv("SMTP_PORT"))
	tls := os.Getenv("SMTP_TLS")
	if tls == "" {
		tls = map[int]string{465: "tls", 25: "none", 1025: "none"}[port]
		if tls == "" {
			tls = "starttls"
		}
	}
	m := mail.SMTP{Host: host, Port: port, Username: os.Getenv("SMTP_USERNAME"), Password: os.Getenv("SMTP_PASSWORD"), From: os.Getenv("SMTP_FROM"), TLS: tls}
	to := os.Getenv("TO")
	if to == "" {
		to = "preview@example.com"
	}
	app := os.Getenv("APP_URL")
	samples := []mail.Message{
		mail.VerifyCode(to, "Rina Wulandari", "482913", 10*time.Minute),
		mail.ResetPassword(to, "Rina Wulandari", app+"/reset-password?token=preview-token", time.Hour),
		mail.NotificationEmail(to, "Rina Wulandari", mail.Notification{Type: "outbid", Title: "Kamu tersalip di Kopi Arabika Garut Q4",
			Body: "harga terbaik sekarang Rp 82.500/kg. Naikkan bid sebelum auction ditutup.", URL: app + "/auctions/demo", Action: "Buka auction"}, app+"/app/settings"),
		mail.NotificationEmail(to, "Rina Wulandari", mail.Notification{Type: "winning_bid", Title: "Kamu memenangkan Box Karton 100.000 pcs",
			Body: "transaksi TRX-4F2A dibuat. Setujui agreement untuk lanjut ke pembayaran.", URL: app + "/app/transactions/demo"}, app+"/app/settings"),
		mail.NotificationEmail(to, "Rina Wulandari", mail.Notification{Type: "payment", Title: "Pembayaran TRX-4F2A masuk escrow",
			Body: "Rp 46.065.000 sudah diterima dan ditahan sampai barang dikonfirmasi diterima.", URL: app + "/app/transactions/demo"}, app+"/app/settings"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, msg := range samples {
		if err := m.Send(ctx, msg); err != nil {
			fmt.Fprintln(os.Stderr, "send:", msg.Subject, err)
			os.Exit(1)
		}
		fmt.Println("sent:", msg.Subject)
	}
}
