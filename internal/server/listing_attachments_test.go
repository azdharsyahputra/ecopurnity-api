package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/azdharsyahputra/ecopurnity-api/internal/storage"
)

var pdfBytes = []byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")

func TestListingAttachments(t *testing.T) {
	e := newEnv(t)
	if e.server.Storage == nil {
		t.Skip("object storage not running (docker compose up seaweedfs)")
	}
	c, _ := e.signedIn("Lampir")
	item := fmt.Sprintf("Kopi lampiran %d", e.scalar(`SELECT (random() * 1e9)::bigint`))
	att := func(r resp) []map[string]any {
		var out []map[string]any
		for _, a := range r.Body["attachments"].([]any) {
			out = append(out, a.(map[string]any))
		}
		return out
	}
	fetch := func(u string) int {
		res, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	create := func(refs ...map[string]any) resp {
		b := supplyBody(item, 90000, 10, "kg")
		b["attachments"] = refs
		return e.call(c, "POST", "/me/listings", b)
	}
	up := func(id string) map[string]any { return map[string]any{"uploadId": id} }
	keep := func(id any) map[string]any { return map[string]any{"id": id} }

	if r := e.call(c, "POST", "/uploads", map[string]any{"purpose": "listing_attachment", "fileName": "a.txt", "contentType": "text/plain", "sizeBytes": 10}); r.Status != 422 ||
		r.field("contentType") != "Format file tidak didukung. Pakai PDF, JPG, PNG, atau WebP." {
		t.Fatalf("type: %d %v", r.Status, r.Body)
	}
	if r := e.call(c, "POST", "/uploads", map[string]any{"purpose": "listing_attachment", "fileName": "a.pdf", "contentType": "application/pdf", "sizeBytes": 11 << 20}); r.Status != 422 ||
		r.field("sizeBytes") != "Ukuran file maksimal 10 MB." {
		t.Fatalf("size: %d %v", r.Status, r.Body)
	}

	photo, doc := e.upload(c, "listing_attachment", "image/png", pngBytes(t)), e.upload(c, "listing_attachment", "application/pdf", pdfBytes)
	r := create(up(photo), up(doc))
	if r.Status != 201 {
		t.Fatalf("create: %d %v", r.Status, r.Body)
	}
	id := r.Body["id"].(string)
	d := e.call(c, "GET", "/me/listings/"+id, nil)
	a := att(d)
	if len(a) != 2 || a[0]["contentType"] != "image/png" || a[1]["contentType"] != "application/pdf" || a[0]["sizeBytes"] != float64(len(pngBytes(t))) {
		t.Fatalf("detail attachments: %v", a)
	}
	for _, x := range a {
		if u, _ := x["url"].(string); u == "" || fetch(u) != 200 {
			t.Fatalf("url: %v", x)
		}
	}
	keys := map[string]string{}
	for _, x := range a {
		keys[x["id"].(string)] = e.scalar(`SELECT object_key FROM listing_attachments WHERE id::text = $1`, x["id"]).(string)
	}

	other, _ := e.signedIn("Bukan Pemilik")
	ktp := e.upload(c, "kyc_ktp", "image/png", pngBytes(t))
	disguised := e.upload(c, "listing_attachment", "image/png", []byte("<html>bukan gambar</html>"))
	nine := make([]map[string]any, 9)
	for i := range nine {
		nine[i] = up("x")
	}
	for name, tc := range map[string]struct {
		refs []map[string]any
		msg  string
	}{
		"reused":      {[]map[string]any{up(photo)}, "File ini sudah dipakai. Unggah ulang."},
		"other user":  {[]map[string]any{up(e.upload(other, "listing_attachment", "image/png", pngBytes(t)))}, "File tidak ditemukan. Unggah ulang."},
		"purpose":     {[]map[string]any{up(ktp)}, "File ini diunggah untuk keperluan lain."},
		"content":     {[]map[string]any{up(disguised)}, "Isi file bukan gambar yang valid."},
		"too many":    {nine, "Maksimal 8 lampiran"},
		"empty ref":   {[]map[string]any{{}}, "Setiap lampiran berisi uploadId atau id."},
		"unknown id":  {[]map[string]any{keep(a[0]["id"])}, "Lampiran tidak ditemukan. Muat ulang halaman."},
		"both in one": {[]map[string]any{{"uploadId": photo, "id": a[0]["id"]}}, "Setiap lampiran berisi uploadId atau id."},
	} {
		if r := create(tc.refs...); r.Status != 422 || r.field("attachments") != tc.msg {
			t.Fatalf("%s: %d %v", name, r.Status, r.Body)
		}
	}

	if n := e.scalar(`SELECT count(*) FROM uploads WHERE id::text = $1 AND consumed_at IS NULL`, ktp); n != int64(1) {
		t.Fatalf("ktp consumed by a failed save: %v", n)
	}

	photo2 := e.upload(c, "listing_attachment", "image/jpeg", jpegBytes(t))
	r = e.call(c, "PATCH", "/me/listings/"+id, map[string]any{"attachments": []map[string]any{keep(a[1]["id"]), up(photo2)}})
	u := att(r)
	if r.Status != 200 || len(u) != 2 || u[0]["id"] != a[1]["id"] || u[1]["contentType"] != "image/jpeg" || u[1]["url"] == nil {
		t.Fatalf("update: %d %v", r.Status, r.Body)
	}
	if _, err := e.server.Storage.Inspect(t0(), keys[a[0]["id"].(string)]); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("removed object still in storage: %v", err)
	}
	if _, err := e.server.Storage.Inspect(t0(), keys[a[1]["id"].(string)]); err != nil {
		t.Fatalf("kept object: %v", err)
	}

	if r := e.call(c, "PATCH", "/me/listings/"+id, map[string]any{"spec": "Grade 2"}); r.Status != 200 || len(att(r)) != 2 {
		t.Fatalf("patch without attachments: %d %v", r.Status, r.Body)
	}

	legacy := e.scalar(`INSERT INTO listing_attachments (listing_id, file_name, content_type, position) VALUES ($1, 'lama.jpg', 'image/jpeg', 2) RETURNING id::text`, id)
	d = e.call(c, "GET", "/me/listings/"+id, nil)
	if a := att(d); len(a) != 3 || a[2]["fileName"] != "lama.jpg" || a[2]["url"] != nil || a[2]["sizeBytes"] != nil {
		t.Fatalf("legacy: %v", a)
	}
	if r := e.call(c, "PATCH", "/me/listings/"+id, map[string]any{"attachments": []map[string]any{keep(legacy), keep(u[0]["id"])}}); r.Status != 200 || len(att(r)) != 2 || att(r)[0]["id"] != legacy {
		t.Fatalf("keep legacy: %d %v", r.Status, r.Body)
	}

	anon := e.client()
	catalog := func() []any {
		r := e.call(anon, "GET", "/listings?q="+url.QueryEscape(item), nil)
		if r.Status != 200 {
			t.Fatalf("catalog: %d %v", r.Status, r.Body)
		}
		return r.Body["data"].([]any)
	}
	pub := catalog()
	if len(pub) != 1 {
		t.Fatalf("catalog rows: %v", pub)
	}
	pa := pub[0].(map[string]any)["attachments"].([]any)
	if len(pa) != 2 || fetch(pa[1].(map[string]any)["url"].(string)) != 200 {
		t.Fatalf("public attachments: %v", pa)
	}
	if r := e.call(c, "POST", "/me/listings/"+id+"/archive", nil); r.Status != 200 {
		t.Fatalf("archive: %d", r.Status)
	}
	if pub := catalog(); len(pub) != 0 {
		t.Fatalf("archived listing still public: %v", pub)
	}
}
