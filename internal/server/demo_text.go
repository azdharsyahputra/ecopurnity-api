package server

import (
	"fmt"
	"strings"
	"unicode"
)

func demoWeighted(g *demoGen, xs []demoMix) string {
	r := g.r.Float64()
	for _, x := range xs {
		if r < x.Weight {
			return x.Key
		}
		r -= x.Weight
	}
	return xs[len(xs)-1].Key
}

type demoName struct {
	Full, First, Last string
	Female            bool
}

func (g *demoGen) personName(province string) demoName {
	e := demoEthnics[demoWeighted(g, demoProvinceMix[province])]
	female := g.chance(0.42)
	first, last := demoPick(g, e.Male), demoPick(g, e.LastM)
	if female {
		first = demoPick(g, e.Female)
		if len(e.LastF) > 0 {
			last = demoPick(g, e.LastF)
		}
	}
	return demoName{Full: first + " " + last, First: first, Last: last, Female: female}
}

func asciiLower(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (g *demoGen) username(n demoName, city string) string {
	parts := strings.Fields(n.Full)
	first, last := asciiLower(parts[0]), asciiLower(parts[len(parts)-1])
	if len(parts) > 2 && (parts[0] == "I" || parts[0] == "Ni") {
		first = asciiLower(parts[1])
	}
	town := asciiLower(strings.Split(city, ",")[0])
	if len(town) > 8 {
		town = town[:8]
	}
	words := []string{"kopi", "tani", "dagang", "kemas", "jaya", "berkah", "usaha", "store"}
	switch g.i(0, 9) {
	case 0, 1:
		return first + "." + last
	case 2:
		return first + last
	case 3:
		return first + "_" + last[:min(3, len(last))]
	case 4:
		return first[:1] + last
	case 5:
		return first + "." + town
	case 6:
		return fmt.Sprintf("%s%d", first, g.i(70, 99))
	case 7:
		return first + "_" + demoPick(g, words)
	case 8:
		return last + "." + first[:1]
	}
	return first + "." + last[:1] + fmt.Sprint(g.i(1, 9))
}

func honorific(p *demoPerson) string {
	first := strings.Fields(p.Name)[0]
	if first == "I" || first == "Ni" {
		first = strings.Fields(p.Name)[1]
	}
	if p.Female {
		return "Bu " + first
	}
	return "Pak " + first
}

var demoBioParts = map[string][]string{
	"agri":          {"Petani kopi dan sayur, jual langsung dari kebun.", "Mengelola lahan 2 ha bersama kelompok tani.", "Pengepul hasil bumi untuk pasar induk."},
	"food":          {"Produksi makanan ringan rumahan sejak 2015.", "Distributor bahan pokok untuk warung sekitar.", "Usaha katering dan oleh-oleh."},
	"packaging":     {"Reseller kemasan untuk UMKM makanan.", "Mengurus pengadaan kemasan di usaha keluarga."},
	"manufacturing": {"Teknisi bubut dan CNC, terima order kecil.", "Penjahit borongan, kapasitas 300 pcs per minggu."},
	"logistics":     {"Punya dua truk engkel, siap antar Jabodetabek.", "Koordinator armada ekspedisi lintas Jawa."},
	"it":            {"Backend engineer freelance, Go dan PostgreSQL.", "Desainer produk digital, fokus aplikasi UMKM.", "Bantu UMKM jualan online lewat iklan dan konten."},
	"energy":        {"Pengepul minyak jelantah dari restoran.", "Teknisi instalasi panel surya."},
}

func (g *demoGen) bio(cat string) string {
	if g.chance(0.3) {
		return ""
	}
	return demoPick(g, demoBioParts[cat])
}

var demoGoodsPraise = map[string][]string{
	"agri": {"kualitasnya sesuai sampel yang dikirim", "sortirannya rapi, hampir tidak ada yang busuk", "kadar airnya pas sesuai kontrak", "ukurannya seragam",
		"hasil cupping sesuai, tidak ada defect berarti", "tidak ada campuran kotoran", "beratnya pas waktu ditimbang ulang"},
	"food": {"rasanya konsisten dengan order sebelumnya", "kemasannya rapi dan tersegel", "masih segar waktu sampai", "beratnya pas, tidak kurang",
		"kualitas beras bersih dan pulen", "label dan tanggal produksi jelas"},
	"packaging": {"cetakannya tajam dan warnanya sesuai proof", "kartonnya tebal dan kuat ditumpuk", "ukurannya presisi", "zipper rapat, tidak bocor",
		"jumlah per bundel pas", "lemnya kuat, tidak ada yang lepas"},
	"manufacturing": {"toleransinya masuk semua", "jahitannya rapi", "finishingnya bersih", "dimensinya sesuai gambar kerja", "material sesuai mill certificate"},
	"logistics":     {"sopirnya sigap dan sopan", "armadanya bersih dan terawat", "tracking GPS jalan terus", "suhu terjaga sepanjang jalan", "bongkar muat cepat"},
	"it":            {"hasil kerjanya rapi dan terdokumentasi", "update progres tiap hari", "deliverable sesuai sprint", "paham kebutuhan bisnis kami", "kode mudah dilanjutkan tim internal"},
	"energy":        {"pemasangannya rapi", "hasil uji lab sesuai", "timnya profesional", "dokumen teknisnya lengkap", "produksi listrik sesuai simulasi"},
}

var demoGoodsIssue = map[string][]string{
	"agri":          {"ada sekitar 3% yang di bawah grade", "beberapa karung kadar airnya agak tinggi", "ada yang mulai layu", "susutnya lebih dari perkiraan"},
	"food":          {"beberapa kemasan penyok", "tanggal produksinya tidak sefresh yang dijanjikan", "ada selisih berat sedikit", "dus luar ada yang basah"},
	"packaging":     {"warna cetak agak beda dari proof", "ada beberapa box yang sobek di sudut", "ikatan per bundel tidak rapi", "ada 1 roll yang lembap"},
	"manufacturing": {"ada 2 part yang harus rework", "beberapa jahitan lepas", "finishing kurang halus", "ukuran XL kurang 10 pcs"},
	"logistics":     {"jemput telat 2 jam", "sopir kurang paham rute", "dokumen jalan kurang lengkap", "update posisi jarang"},
	"it":            {"beberapa revisi molor", "dokumentasi kurang lengkap", "respon di akhir pekan lambat"},
	"energy":        {"jadwal pemasangan mundur", "ada selisih kadar dengan sampel", "kabel kurang dirapikan"},
}

var demoDeliveryPraise = []string{"Pengiriman tepat waktu.", "Sampai sesuai jadwal.", "Packing aman, tidak ada yang rusak.", "Diantar sampai gudang dan dibantu bongkar.",
	"Lebih cepat sehari dari estimasi.", "Surat jalan dan invoice lengkap.", ""}
var demoClosers = []string{"Lanjut order bulan depan.", "Bakal repeat order.", "Terima kasih, sukses terus!", "Recommended.", "Mantap.", "Sudah langganan, tidak pernah kecewa.", ""}
var demoBuyerPraise = []string{"Pembayaran cepat, langsung masuk escrow.", "Pembeli komunikatif, alamat dan jam bongkar jelas.", "Konfirmasi terima barang cepat, prosesnya lancar.",
	"Spesifikasi dari pembeli detail sehingga produksi tidak salah.", "Kerja sama yang enak, semoga berlanjut.", "Gudang pembeli tertata, bongkar cepat.", "Mantap, pembeli ramah dan tepat janji.",
	"Pembayaran sesuai termin, tidak perlu ditagih.", "Respons cepat waktu kami tanya alamat kirim.", "Terima kasih atas ordernya, ditunggu repeat."}
var demoBuyerIssue = []string{"Konfirmasi penerimaan agak lama tapi akhirnya beres.", "Jam bongkar sempat berubah mendadak.", "Revisi spek di tengah jalan, untung bisa disesuaikan.",
	"Pembayaran mundur beberapa hari dari jatuh tempo."}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func (g *demoGen) reviewText(cat, item string, rating int, ofSupplier bool) string {
	return strings.Join(strings.Fields(g.rawReview(cat, item, rating, ofSupplier)), " ")
}

func (g *demoGen) rawReview(cat, item string, rating int, ofSupplier bool) string {
	if !ofSupplier {
		if rating >= 4 {
			return demoPick(g, demoBuyerPraise)
		}
		return demoPick(g, demoBuyerIssue)
	}
	praise, issue := demoPick(g, demoGoodsPraise[cat]), demoPick(g, demoGoodsIssue[cat])
	switch {
	case rating >= 5:
		forms := []string{
			"%[1]s, %[2]s. %[3]s %[4]s",
			"%[4]s %[1]s dari supplier ini selalu konsisten, %[2]s.",
			"Order %[1]s kedua kalinya, tetap %[2]s. %[3]s",
			"Puas. %[5]s, %[2]s.",
			"Top! %[3]s",
			"%[1]s oke, %[2]s. Komunikasi enak dan responsif. %[4]s",
		}
		return strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(demoPick(g, forms), item, praise, demoPick(g, demoDeliveryPraise), demoPick(g, demoClosers), capFirst(praise)), "  ", " "))
	case rating == 4:
		forms := []string{
			"Secara umum bagus, %[1]s. Cuma %[2]s.",
			"%[3]s sesuai pesanan, %[1]s. Catatan kecil: %[2]s.",
			"Oke, %[1]s. Minus sedikit karena %[2]s, tapi masih bisa diterima.",
			"Bagus. %[4]s Hanya saja %[2]s.",
		}
		return fmt.Sprintf(demoPick(g, forms), praise, issue, item, demoPick(g, demoDeliveryPraise))
	case rating == 3:
		return fmt.Sprintf(demoPick(g, []string{"Cukup. %s, pengiriman juga mundur sehari. Perlu dicek lagi sebelum kirim.",
			"Biasa saja. %s dan harus dikomplain dulu baru ada solusi.", "%s. Semoga order berikutnya lebih teliti."}), capFirst(issue))
	}
	if g.chance(0.5) {
		return fmt.Sprintf("Kecewa, %s. Komunikasi juga lambat waktu dikomplain.", issue)
	}
	return fmt.Sprintf("Tidak sesuai harapan: %s. Untuk %s kami cari supplier lain dulu.", issue, item)
}

var demoDisputeReasons = map[string][]string{
	"agri":          {"Kadar air di atas spek (uji kami 15%, kontrak maks. 12,5%)", "Sekitar 8% barang busuk saat dibongkar", "Grade tidak sesuai sampel yang disetujui"},
	"food":          {"Sebagian kemasan bocor dan tidak layak jual", "Berat bersih per karung kurang 1–2 kg", "Tanggal kedaluwarsa terlalu dekat dari yang dijanjikan"},
	"packaging":     {"Warna cetak jauh dari proof yang disetujui", "Box penyok dan basah karena terkena hujan saat kirim", "Jumlah per bundel kurang dari surat jalan"},
	"manufacturing": {"30% part di luar toleransi gambar kerja", "Ukuran seragam tidak sesuai size chart", "Material berbeda dari penawaran"},
	"logistics":     {"Muatan rusak karena suhu tidak terjaga", "Pengiriman terlambat 3 hari tanpa kabar", "Barang tertinggal di gudang transit"},
	"it":            {"Fitur yang diserahkan belum sesuai lingkup kontrak", "Jam kerja yang ditagih tidak sesuai timesheet"},
	"energy":        {"Daya keluaran inverter jauh di bawah spesifikasi", "Kadar FFA di atas yang disepakati"},
}

var demoSupplierDefense = []string{"Barang dikirim sesuai PO; terlampir foto saat muat dan hasil uji internal kami.", "Kerusakan terjadi di perjalanan oleh ekspedisi pihak pembeli, bukan dari kami.",
	"Kami siap kirim pengganti untuk bagian yang tidak sesuai, mohon dicek ulang bersama.", "Sampel yang disetujui sama dengan barang yang dikirim, lihat foto terlampir."}

func (g *demoGen) disputeReason(cat string) string { return demoPick(g, demoDisputeReasons[cat]) }

func (g *demoGen) rfqChat(buyer, supplier *demoPerson, item, unit, city string, qty float64, price int64, spec string) []string {
	h := honorific(supplier)
	hb := honorific(buyer)
	greet := demoPick(g, []string{"Selamat pagi", "Siang", "Assalamualaikum", "Halo", "Selamat siang"})
	lines := []string{
		fmt.Sprintf("%s %s, kami butuh %s %s %s, dikirim ke %s. Bisa dibantu penawarannya?", greet, h, idNumber(qty), unit, item, strings.Split(city, ",")[0]),
		fmt.Sprintf("Bisa %s. Untuk spek %s, harga kami %s per %s franco lokasi.", hb, spec, rupiah(price), unit),
	}
	more := [][]string{
		{"Pembayarannya bisa termin 14 hari?", "Untuk order pertama kami minta escrow dulu ya, berikutnya bisa net 14."},
		{"Lead time berapa hari?", fmt.Sprintf("Siap kirim %d hari kerja setelah PO, bisa bertahap.", g.i(2, 7))},
		{"Ada sertifikat atau hasil uji lab?", "Ada, nanti kami lampirkan bersama surat jalan."},
		{fmt.Sprintf("Kalau ambil rutin tiap bulan bisa %s?", rupiah(nicePrice(float64(price)*0.96))), "Bisa kami pertimbangkan untuk kontrak minimal 3 bulan."},
		{"Bisa kirim sampel dulu?", "Bisa, sampel 1 kg kami kirim besok pagi."},
	}
	for _, k := range g.r.Perm(len(more))[:g.i(1, 2)] {
		lines = append(lines, more[k]...)
	}
	lines = append(lines, demoPick(g, []string{"Oke, saya ajukan ke atasan dulu ya.", "Siap, kami tunggu quote resminya.", "Baik, terima kasih infonya.", "Sip, kami bandingkan dulu dengan supplier lain."}))
	return lines
}

func (g *demoGen) tradeChat(buyer, supplier *demoPerson, item, unit string, qty float64) []string {
	hs, hb := honorific(supplier), honorific(buyer)
	opts := [][]string{
		{fmt.Sprintf("%s, %s %s %s sudah siap kirim?", hs, idNumber(qty), unit, item), fmt.Sprintf("Sudah %s, besok pagi berangkat. Nanti saya kirim foto surat jalan.", hb), "Siap, ditunggu. Gudang buka jam 8."},
		{"Barang sudah sampai, sedang kami QC dulu.", "Baik, kalau ada temuan langsung kabari ya.", "Aman semua, sudah kami konfirmasi terima. Terima kasih!"},
		{fmt.Sprintf("%s, invoice sudah terbit ya, mohon dicek.", hb), "Sudah kami bayar lewat escrow barusan.", "Terkonfirmasi, kami proses pengiriman."},
		{"Bisa dikirim bertahap? Gudang kami terbatas.", "Bisa, kami bagi dua pengiriman minggu ini dan minggu depan.", "Oke, cocok."},
	}
	return demoPick(g, opts)
}

var demoQuoteNotes = []string{"Harga franco gudang pembeli, belum termasuk PPN.", "Stok ready, bisa kirim bertahap.", "Harga berlaku 7 hari.",
	"Lead time 5 hari kerja setelah PO.", "Sudah termasuk ongkos bongkar.", "Pembayaran escrow, sisa dokumen menyusul.", "Bisa kirim sampel sebelum PO."}
