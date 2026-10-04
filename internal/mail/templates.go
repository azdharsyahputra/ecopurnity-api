package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"strings"
	texttemplate "text/template"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var names = []string{"verify_code", "reset_password", "notification"}

type button struct{ URL, Label string }

func makeButton(url, label string) button { return button{url, label} }

var (
	htmlPages = map[string]*htmltemplate.Template{}
	textPages = map[string]*texttemplate.Template{}
)

func init() {
	for _, n := range names {
		htmlPages[n] = htmltemplate.Must(htmltemplate.New(n).Funcs(htmltemplate.FuncMap{"button": makeButton}).
			ParseFS(templateFS, "templates/layout.html", "templates/"+n+".html"))
		textPages[n] = texttemplate.Must(texttemplate.New(n).Funcs(texttemplate.FuncMap{"button": makeButton}).
			ParseFS(templateFS, "templates/"+n+".html"))
	}
}

type page struct {
	Subject, Preheader, Footer, ManageURL string
	Page                                  any
}

func render(name, to string, data any, footer, manageURL string) (Message, error) {
	exec := func(def string) (string, error) {
		var b bytes.Buffer
		err := textPages[name].ExecuteTemplate(&b, def, data)
		return strings.TrimSpace(b.String()), err
	}
	subject, err := exec("subject")
	if err != nil {
		return Message{}, err
	}
	pre, err := exec("preheader")
	if err != nil {
		return Message{}, err
	}
	text, err := exec("text")
	if err != nil {
		return Message{}, err
	}
	var body bytes.Buffer
	if err := htmlPages[name].ExecuteTemplate(&body, "layout", page{Subject: subject, Preheader: pre, Footer: footer, ManageURL: manageURL, Page: data}); err != nil {
		return Message{}, err
	}
	return Message{To: to, Subject: subject, HTML: body.String(), Body: text + "\n"}, nil
}

func must(m Message, err error) Message {
	if err != nil {
		panic(fmt.Sprintf("mail template: %v", err))
	}
	return m
}

func VerifyCode(to, name, code string, ttl time.Duration) Message {
	m := must(render("verify_code", to, struct {
		Name, Code string
		TTLMinutes int
	}{name, code, int(ttl.Minutes())}, "", ""))
	m.Code = code
	return m
}

func ResetPassword(to, name, url string, ttl time.Duration) Message {
	m := must(render("reset_password", to, struct {
		Name, URL  string
		TTLMinutes int
	}{name, url, int(ttl.Minutes())}, "Kamu menerima email ini karena ada permintaan reset password untuk akunmu.", ""))
	m.Link = url
	return m
}

type Notification struct {
	Type   string
	Title  string
	Body   string
	URL    string
	Action string
}

var notificationTags = map[string][3]string{
	"outbid":               {"Tersalip", "#fdecec", "#b42318"},
	"winning_bid":          {"Menang", "#e8f6ee", "#18794e"},
	"auction_ending":       {"Auction", "#fff4e5", "#b54708"},
	"auction_invitation":   {"Undangan", "#eef2ff", "#3d4db7"},
	"payment":              {"Pembayaran", "#e8f6ee", "#18794e"},
	"delivery":             {"Pengiriman", "#e7f5f8", "#00586a"},
	"transaction_update":   {"Transaksi", "#e7f5f8", "#00586a"},
	"new_market":           {"Market baru", "#f3eefc", "#6941c6"},
	"opportunity_detected": {"Opportunity", "#f3f8e6", "#4d7c0f"},
	"reputation_update":    {"Reputasi", "#f2f4f7", "#344054"},
}

func NotificationEmail(to, name string, n Notification, manageURL string) Message {
	tag := notificationTags[n.Type]
	if n.Action == "" {
		n.Action = "Lihat detail"
	}
	return must(render("notification", to, struct {
		Notification
		Name, Tag, TagBg, TagFg, ManageURL string
	}{n, name, tag[0], tag[1], tag[2], manageURL}, "", manageURL))
}
