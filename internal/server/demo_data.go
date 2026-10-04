package server

const DemoEmailDomain = "demo.ecopurnity.my.id"

type demoEthnic struct {
	Male, Female, LastM, LastF []string
}

var demoEthnics = map[string]demoEthnic{
	"sunda": {
		Male:   []string{"Asep", "Cecep", "Ujang", "Dadang", "Yayan", "Aep", "Deden", "Tatang", "Dede", "Ade", "Wawan", "Iwan", "Rudi", "Agus", "Hendra"},
		Female: []string{"Euis", "Neneng", "Iis", "Rina", "Ai", "Yeni", "Nining", "Lilis", "Tita", "Rika", "Siti", "Nur"},
		LastM:  []string{"Sunarya", "Permana", "Kurniawan", "Hidayat", "Supriatna", "Rustandi", "Mulyana", "Gumilar", "Saepudin", "Ruhimat", "Kusmana", "Sutisna"},
		LastF:  []string{"Nurjanah", "Rahmawati", "Kurniasih", "Komalasari", "Sumiati", "Sartika", "Rosmiati", "Hasanah"},
	},
	"jawa": {
		Male:   []string{"Bambang", "Joko", "Budi", "Agus", "Wahyu", "Slamet", "Sugeng", "Bagus", "Dimas", "Galih", "Teguh", "Heru", "Eko", "Yudi", "Arif"},
		Female: []string{"Sri", "Endang", "Suharti", "Rini", "Puji", "Ayu", "Wulan", "Dwi", "Tri", "Retno", "Siti", "Indah"},
		LastM:  []string{"Santoso", "Wibowo", "Prasetyo", "Purnomo", "Susilo", "Sulistyo", "Setiawan", "Nugroho", "Kuncoro", "Hartono", "Utomo", "Saputro"},
		LastF:  []string{"Rahayu", "Handayani", "Wijayanti", "Wardhani", "Lestari", "Susanti", "Kusumawati", "Purwaningsih"},
	},
	"batak": {
		Male:   []string{"Togar", "Parlindungan", "Lamhot", "Hotman", "Sahat", "Marihot", "Benny", "Rudolf", "Jansen", "Poltak"},
		Female: []string{"Rosmauli", "Duma", "Tiurma", "Ruth", "Melva", "Rosa", "Lasmaria", "Hotnida"},
		LastM:  []string{"Siregar", "Simanjuntak", "Nasution", "Hutapea", "Sinaga", "Sembiring", "Pangaribuan", "Lubis", "Situmorang", "Tambunan", "Hasibuan", "Ginting"},
	},
	"minang": {
		Male:   []string{"Rizal", "Afdal", "Fadli", "Zulkifli", "Syafri", "Irwan", "Yusrizal", "Hendri"},
		Female: []string{"Nurhayati", "Rahmi", "Yusra", "Fitria", "Desmawati", "Elvi"},
		LastM:  []string{"Chaniago", "Piliang", "Koto", "Tanjung", "Sikumbang", "Jambak"},
	},
	"bugis": {
		Male:   []string{"Andi Baso", "Muhammad Ilham", "Ambo", "Andi Arfan", "Haerul", "Syamsul"},
		Female: []string{"Sitti Hasnah", "Besse", "Andi Tenri", "Nurul", "Hasmiati"},
		LastM:  []string{"Rahman", "Syarif", "Latif", "Hamzah", "Pawelloi", "Basri"},
	},
	"bali": {
		Male:   []string{"I Wayan", "I Made", "I Nyoman", "I Ketut", "I Gede", "I Komang"},
		Female: []string{"Ni Wayan", "Ni Made", "Ni Luh", "Ni Ketut", "Ni Putu", "Ni Komang"},
		LastM:  []string{"Sudarma", "Arsana", "Wirawan", "Astawa", "Suardana", "Budiarta", "Mahendra", "Sukadana"},
		LastF:  []string{"Ariani", "Sukerti", "Widiastuti", "Sriasih", "Purnami", "Darmayanti"},
	},
	"tionghoa": {
		Male:   []string{"Hendra", "Yohanes", "Kevin", "Andreas", "Wilson", "Hartono", "Stevan", "Ferry"},
		Female: []string{"Liliana", "Felicia", "Stefanie", "Meliana", "Lina", "Jessica", "Vivian", "Susi"},
		LastM:  []string{"Halim", "Tjandra", "Gunawan", "Setiadi", "Hartanto", "Sutanto", "Lim", "Tan", "Gozali", "Wijaya"},
	},
}

type demoMix struct {
	Key    string
	Weight float64
}

var demoProvinceMix = map[string][]demoMix{
	"Jawa Barat":     {{"sunda", 0.72}, {"jawa", 0.1}, {"tionghoa", 0.08}, {"minang", 0.06}, {"batak", 0.04}},
	"Banten":         {{"sunda", 0.5}, {"jawa", 0.2}, {"tionghoa", 0.15}, {"minang", 0.1}, {"bugis", 0.05}},
	"DKI Jakarta":    {{"jawa", 0.3}, {"sunda", 0.15}, {"tionghoa", 0.2}, {"batak", 0.12}, {"minang", 0.13}, {"bugis", 0.1}},
	"Jawa Tengah":    {{"jawa", 0.85}, {"tionghoa", 0.1}, {"sunda", 0.05}},
	"DI Yogyakarta":  {{"jawa", 0.8}, {"tionghoa", 0.08}, {"minang", 0.06}, {"batak", 0.06}},
	"Jawa Timur":     {{"jawa", 0.78}, {"tionghoa", 0.12}, {"bugis", 0.05}, {"minang", 0.05}},
	"Bali":           {{"bali", 0.85}, {"jawa", 0.1}, {"tionghoa", 0.05}},
	"Sumatera Utara": {{"batak", 0.7}, {"tionghoa", 0.15}, {"minang", 0.15}},
}

var demoProvinceCities = map[string][]string{
	"Jawa Barat": {"Bandung, Jawa Barat", "Garut, Jawa Barat", "Bogor, Jawa Barat", "Bekasi, Jawa Barat", "Depok, Jawa Barat", "Karawang, Jawa Barat",
		"Cirebon, Jawa Barat", "Tasikmalaya, Jawa Barat", "Sukabumi, Jawa Barat", "Cianjur, Jawa Barat", "Indramayu, Jawa Barat", "Subang, Jawa Barat",
		"Purwakarta, Jawa Barat", "Pangalengan, Kab. Bandung, Jawa Barat", "Cikarang, Kab. Bekasi, Jawa Barat"},
	"Banten":        {"Tangerang, Banten", "Serang, Banten", "Cilegon, Banten"},
	"DKI Jakarta":   {"Jakarta Selatan, DKI Jakarta", "Jakarta Barat, DKI Jakarta", "Jakarta Utara, DKI Jakarta", "Jakarta Timur, DKI Jakarta"},
	"Jawa Tengah":   {"Semarang, Jawa Tengah", "Surakarta, Jawa Tengah", "Magelang, Jawa Tengah", "Brebes, Jawa Tengah", "Temanggung, Jawa Tengah", "Klaten, Jawa Tengah", "Boyolali, Jawa Tengah", "Pati, Jawa Tengah", "Kudus, Jawa Tengah", "Pekalongan, Jawa Tengah", "Wonosobo, Jawa Tengah"},
	"DI Yogyakarta": {"Yogyakarta, DI Yogyakarta", "Sleman, DI Yogyakarta", "Bantul, DI Yogyakarta"},
	"Jawa Timur": {"Surabaya, Jawa Timur", "Malang, Jawa Timur", "Sidoarjo, Jawa Timur", "Gresik, Jawa Timur", "Jember, Jawa Timur", "Banyuwangi, Jawa Timur",
		"Ngawi, Jawa Timur", "Kediri, Jawa Timur", "Probolinggo, Jawa Timur"},
	"Bali":           {"Denpasar, Bali", "Gianyar, Bali", "Tabanan, Bali", "Kintamani, Bangli, Bali", "Badung, Bali", "Buleleng, Bali"},
	"Sumatera Utara": {"Medan, Sumatera Utara", "Balige, Toba, Sumatera Utara", "Deli Serdang, Sumatera Utara", "Pematangsiantar, Sumatera Utara", "Berastagi, Karo, Sumatera Utara", "Sidikalang, Sumatera Utara"},
}

var demoProvinceWeight = []demoMix{{"Jawa Barat", 0.3}, {"DKI Jakarta", 0.12}, {"Banten", 0.06}, {"Jawa Tengah", 0.17}, {"DI Yogyakarta", 0.08},
	{"Jawa Timur", 0.15}, {"Bali", 0.06}, {"Sumatera Utara", 0.06}}

type demoItem struct {
	Name, Category, Unit string
	PriceMin, PriceMax   int64
	QtyMin, QtyMax       float64
	Cities               []string
	Specs                []string
	Delivery             string
	DemandBias           float64
}

var demoItems = []demoItem{
	{"Kopi arabika Gayo green bean", "agri", "kg", 115_000, 150_000, 100, 3_000, []string{"Medan, Sumatera Utara", "Sidikalang, Sumatera Utara"},
		[]string{"Grade 1, giling basah, kadar air 12–13%, triple pick, karung goni 60 kg", "Semi-washed, screen 16 up, defect maks. 11 per 300 g"}, "deliver", 0.4},
	{"Kopi arabika Java Preanger green bean", "agri", "kg", 110_000, 140_000, 100, 3_000, []string{"Garut, Jawa Barat", "Pangalengan, Kab. Bandung, Jawa Barat", "Bandung, Jawa Barat"},
		[]string{"Full wash, kadar air ≤ 12,5%, defect maks. 11 per 300 g", "Honey process, petik merah, panen raya Juni–Agustus", "Natural anaerob 72 jam, cupping 84+"}, "both", 0.45},
	{"Kopi arabika Kintamani green bean", "agri", "kg", 120_000, 150_000, 80, 2_000, []string{"Kintamani, Bangli, Bali"},
		[]string{"Washed, IG Kopi Kintamani Bali, kadar air ≤ 12%", "Fermentasi 36 jam, screen 15 up"}, "deliver", 0.4},
	{"Kopi robusta Temanggung green bean", "agri", "kg", 58_000, 72_000, 200, 6_000, []string{"Temanggung, Jawa Tengah", "Magelang, Jawa Tengah"},
		[]string{"Petik merah ≥ 90%, kadar air ≤ 13%, karung 60 kg", "Natural, screen 16, triase basah"}, "deliver", 0.4},
	{"Cabai rawit merah", "agri", "kg", 35_000, 68_000, 100, 2_000, []string{"Garut, Jawa Barat", "Magelang, Jawa Tengah", "Brebes, Jawa Tengah"},
		[]string{"Petik H-1, tanpa tangkai, sortir busuk", "Rawit setan, ukuran seragam, karung jaring 10 kg"}, "deliver", 0.55},
	{"Bawang merah Brebes", "agri", "kg", 28_000, 42_000, 200, 5_000, []string{"Brebes, Jawa Tengah"},
		[]string{"Bima Brebes, kering askip, 40–50 umbi/kg", "Kering rogol, susut maks. 5%"}, "deliver", 0.5},
	{"Kentang granola", "agri", "kg", 10_000, 14_500, 300, 6_000, []string{"Pangalengan, Kab. Bandung, Jawa Barat", "Berastagi, Karo, Sumatera Utara", "Wonosobo, Jawa Tengah"},
		[]string{"Grade AB, 8–12 umbi/kg, kulit mulus", "Grade super untuk keripik, kadar gula rendah"}, "deliver", 0.5},
	{"Jagung pipil kering", "agri", "kg", 5_000, 6_200, 2_000, 30_000, []string{"Jember, Jawa Timur", "Kediri, Jawa Timur", "Malang, Jawa Timur"},
		[]string{"Kadar air ≤ 15%, aflatoksin < 50 ppb", "Untuk pakan ternak, karung 50 kg"}, "deliver", 0.45},
	{"Pupuk organik granul", "agri", "kg", 1_600, 2_400, 2_000, 40_000, []string{"Magelang, Jawa Tengah", "Klaten, Jawa Tengah", "Boyolali, Jawa Tengah"},
		[]string{"C-organik ≥ 15%, SNI 7763:2018, karung 40 kg", "Granul 2–5 mm, kadar air ≤ 15%, berlabel Kementan"}, "deliver", 0.65},
	{"Singkong segar", "agri", "kg", 1_500, 2_400, 3_000, 40_000, []string{"Pati, Jawa Tengah", "Kediri, Jawa Timur"},
		[]string{"Umur panen 9–10 bulan, kadar pati ≥ 25%", "Tanpa bonggol, maks. 2 hari setelah cabut"}, "deliver", 0.5},
	{"Beras medium", "food", "kg", 12_400, 13_600, 2_000, 40_000, []string{"Karawang, Jawa Barat", "Indramayu, Jawa Barat", "Ngawi, Jawa Timur", "Subang, Jawa Barat"},
		[]string{"IR64, broken ≤ 20%, kemasan 25 kg", "Ciherang panen gadu, karung 50 kg"}, "deliver", 0.45},
	{"Beras premium", "food", "kg", 14_500, 15_800, 1_000, 20_000, []string{"Karawang, Jawa Barat", "Ngawi, Jawa Timur"},
		[]string{"Broken ≤ 15%, pulen, kemasan 5 kg", "Pandan wangi Cianjur, kemasan 10 kg"}, "deliver", 0.5},
	{"Gula aren cetak", "food", "kg", 24_000, 33_000, 200, 5_000, []string{"Balige, Toba, Sumatera Utara", "Bantul, DI Yogyakarta"},
		[]string{"Kadar air ≤ 10%, cetak batok, dus 20 kg", "Murni nira aren tanpa campuran gula pasir"}, "both", 0.35},
	{"Gula semut aren", "food", "kg", 35_000, 48_000, 100, 3_000, []string{"Balige, Toba, Sumatera Utara", "Bantul, DI Yogyakarta"},
		[]string{"Mesh 20, kadar air ≤ 3%, kemasan 25 kg", "Organik, siap ekspor"}, "deliver", 0.4},
	{"Minyak goreng curah", "food", "liter", 15_000, 17_000, 1_000, 15_000, []string{"Surabaya, Jawa Timur", "Medan, Sumatera Utara"},
		[]string{"Sawit, jerigen 18 liter", "Bening, FFA ≤ 0,3%, drum 190 kg"}, "deliver", 0.6},
	{"Telur ayam ras", "food", "kg", 25_000, 29_000, 200, 3_000, []string{"Bogor, Jawa Barat", "Bekasi, Jawa Barat", "Kediri, Jawa Timur"},
		[]string{"Grade A, peti 15 kg", "Umur maks. 3 hari, ukuran seragam 16 butir/kg"}, "deliver", 0.55},
	{"Tepung tapioka", "food", "kg", 7_500, 9_500, 1_000, 20_000, []string{"Pati, Jawa Tengah", "Bogor, Jawa Barat"},
		[]string{"Grade A, derajat putih ≥ 92%, karung 50 kg", "Kadar air ≤ 13%, untuk kerupuk"}, "deliver", 0.5},
	{"Keripik singkong balado", "food", "kg", 40_000, 60_000, 50, 800, []string{"Tasikmalaya, Jawa Barat", "Bandung, Jawa Barat"},
		[]string{"Kemasan 250 g, PIRT, halal MUI", "Curah 5 kg untuk reseller"}, "both", 0.45},
	{"Ikan tongkol beku", "food", "kg", 22_000, 30_000, 300, 5_000, []string{"Sidoarjo, Jawa Timur", "Banyuwangi, Jawa Timur"},
		[]string{"Beku −18 °C, 300–500 g/ekor", "Utuh, sudah disiangi, master carton 20 kg"}, "deliver", 0.5},
	{"Box karton double wall 40×30×20 cm", "packaging", "pcs", 2_600, 3_800, 5_000, 80_000, []string{"Tangerang, Banten", "Sidoarjo, Jawa Timur", "Bekasi, Jawa Barat"},
		[]string{"Flute BC, K150/M125/K150, cetak 1 warna", "Polos, ikat 25 pcs, kirim palet"}, "deliver", 0.6},
	{"Standing pouch zipper 250 g", "packaging", "pcs", 1_100, 1_800, 5_000, 100_000, []string{"Bandung, Jawa Barat", "Sidoarjo, Jawa Timur"},
		[]string{"PET/PE food grade, zipper, cetak digital 3 warna", "Kraft window, zipper, dasar berdiri"}, "deliver", 0.7},
	{"Karung plastik 50 kg", "packaging", "lembar", 2_300, 3_200, 2_000, 40_000, []string{"Sidoarjo, Jawa Timur", "Gresik, Jawa Timur", "Tangerang, Banten"},
		[]string{"Woven PP 55×90 cm, denier 900", "Laminasi, tahan air, jahit bawah"}, "deliver", 0.55},
	{"Botol PET 600 ml", "packaging", "pcs", 850, 1_250, 10_000, 120_000, []string{"Tangerang, Banten", "Bekasi, Jawa Barat"},
		[]string{"Bening, neck 28 mm PCO, food grade", "Termasuk tutup ulir, kemasan bal 500 pcs"}, "deliver", 0.5},
	{"Kertas kraft liner", "packaging", "kg", 9_000, 12_000, 2_000, 30_000, []string{"Tangerang, Banten", "Gresik, Jawa Timur"},
		[]string{"125 gsm, roll lebar 150 cm", "Recycled, bursting strength ≥ 16 kgf/cm²"}, "deliver", 0.5},
	{"Paper bag kraft", "packaging", "pcs", 1_600, 2_800, 3_000, 50_000, []string{"Bandung, Jawa Barat", "Jakarta Barat, DKI Jakarta"},
		[]string{"Kraft 120 gsm, tali kertas, sablon 1 warna", "25×12×30 cm, tanpa tali"}, "both", 0.6},
	{"Jasa CNC machining", "manufacturing", "jam", 175_000, 300_000, 20, 400, []string{"Cikarang, Kab. Bekasi, Jawa Barat", "Karawang, Jawa Barat"},
		[]string{"Toleransi ±0,02 mm, aluminium dan baja karbon", "Prototipe dan produksi batch kecil, gambar kerja dari customer"}, "pickup", 0.55},
	{"Plat besi hitam 2 mm 4×8 ft", "manufacturing", "lembar", 880_000, 1_050_000, 20, 600, []string{"Cikarang, Kab. Bekasi, Jawa Barat", "Surabaya, Jawa Timur", "Gresik, Jawa Timur"},
		[]string{"SNI, tebal aktual 1,9–2,0 mm", "Bisa potong sesuai ukuran, mill certificate"}, "deliver", 0.5},
	{"Biji plastik PP daur ulang", "manufacturing", "kg", 12_000, 15_500, 1_000, 20_000, []string{"Tangerang, Banten", "Bekasi, Jawa Barat", "Sidoarjo, Jawa Timur"},
		[]string{"MFI 10–12, warna natural", "Pelet bersih, kadar air ≤ 0,2%, karung 25 kg"}, "deliver", 0.5},
	{"Jasa konveksi seragam kerja", "manufacturing", "pcs", 85_000, 130_000, 100, 3_000, []string{"Surakarta, Jawa Tengah", "Bandung, Jawa Barat"},
		[]string{"Kain drill Japan, bordir logo dada", "Kemeja PDH lengan panjang, ukuran S–XXL"}, "both", 0.6},
	{"Truk engkel Jabodetabek", "logistics", "trip", 850_000, 1_400_000, 4, 80, []string{"Jakarta Utara, DKI Jakarta", "Bekasi, Jawa Barat", "Tangerang, Banten"},
		[]string{"Engkel box 2 ton, sopir + kenek", "Termasuk tol dan BBM, maks. 3 titik drop"}, "both", 0.55},
	{"Truk fuso Jakarta–Medan", "logistics", "trip", 14_000_000, 19_000_000, 1, 12, []string{"Jakarta Utara, DKI Jakarta", "Medan, Sumatera Utara"},
		[]string{"Fuso box 8 ton via Merak–Bakauheni, 5–6 hari", "Termasuk penyeberangan, tol, dan asuransi muatan"}, "both", 0.5},
	{"Truk berpendingin Surabaya–Malang", "logistics", "trip", 1_800_000, 2_600_000, 4, 60, []string{"Surabaya, Jawa Timur", "Malang, Jawa Timur"},
		[]string{"Suhu −18 °C, GPS tracking", "Asuransi muatan, data logger suhu"}, "both", 0.6},
	{"Sewa gudang per pallet", "logistics", "pallet", 60_000, 110_000, 20, 400, []string{"Cikarang, Kab. Bekasi, Jawa Barat", "Karawang, Jawa Barat", "Gresik, Jawa Timur"},
		[]string{"Per pallet per bulan, racking, forklift", "Gudang kering, CCTV 24 jam"}, "both", 0.5},
	{"Backend developer Go", "it", "jam", 150_000, 280_000, 20, 400, []string{"Yogyakarta, DI Yogyakarta", "Sleman, DI Yogyakarta", "Bandung, Jawa Barat", "Jakarta Selatan, DKI Jakarta"},
		[]string{"Go, PostgreSQL, REST/gRPC, pengalaman ≥ 3 tahun", "Remote, overlap jam kerja WIB, code review"}, "pickup", 0.6},
	{"Desain UI/UX aplikasi", "it", "jam", 125_000, 240_000, 20, 300, []string{"Bandung, Jawa Barat", "Jakarta Selatan, DKI Jakarta", "Yogyakarta, DI Yogyakarta"},
		[]string{"Figma, design system, prototipe klik", "Riset pengguna dan usability test 5 responden"}, "pickup", 0.55},
	{"Jasa digital marketing UMKM", "it", "jam", 90_000, 160_000, 10, 200, []string{"Yogyakarta, DI Yogyakarta", "Denpasar, Bali", "Bandung, Jawa Barat"},
		[]string{"Iklan Meta dan marketplace, laporan mingguan", "Foto produk dan copywriting"}, "pickup", 0.5},
	{"Instalasi jaringan kantor", "it", "titik", 350_000, 600_000, 10, 120, []string{"Jakarta Selatan, DKI Jakarta", "Surabaya, Jawa Timur"},
		[]string{"Kabel Cat6, termasuk konektor dan tes", "Rak 19 inci, garansi 1 tahun"}, "pickup", 0.6},
	{"Paket PLTS atap 3 kWp on-grid", "energy", "unit", 38_000_000, 52_000_000, 2, 30, []string{"Denpasar, Bali", "Badung, Bali", "Gianyar, Bali"},
		[]string{"Panel mono 550 Wp, inverter on-grid, garansi 10 tahun", "Termasuk instalasi, SLO, dan pengajuan ekspor-impor PLN"}, "deliver", 0.6},
	{"Minyak jelantah (UCO)", "energy", "kg", 7_000, 10_000, 300, 6_000, []string{"Surabaya, Jawa Timur", "Malang, Jawa Timur", "Jakarta Timur, DKI Jakarta"},
		[]string{"FFA ≤ 5%, kadar air ≤ 1%, jerigen 18 liter", "Disaring, bebas endapan, ambil di lokasi"}, "pickup", 0.4},
	{"Briket arang tempurung kelapa", "energy", "kg", 17_000, 24_000, 500, 10_000, []string{"Bantul, DI Yogyakarta", "Medan, Sumatera Utara", "Banyuwangi, Jawa Timur"},
		[]string{"Kubus 2,5 cm, kadar abu ≤ 2,5%, untuk shisha", "Hexagonal, kalori ≥ 7.000 kcal/kg"}, "deliver", 0.4},
	{"Pelet kayu biomassa", "energy", "kg", 1_800, 2_600, 5_000, 60_000, []string{"Semarang, Jawa Tengah", "Jember, Jawa Timur"},
		[]string{"Diameter 8 mm, kalori ≥ 4.200 kcal/kg", "Jumbo bag 1 ton, kadar air ≤ 10%"}, "deliver", 0.5},
}

type demoOrgDef struct {
	Name, Type, City, Industry, Verification string
	Supply, Buy                              []string
	Desc                                     string
	MM                                       bool
}

var demoMMOrgs = []demoOrgDef{
	{"Koperasi Produsen Kopi Priangan Garut", "Koperasi", "Garut, Jawa Barat", "Koperasi · Pertanian", "verified", []string{"agri", "food"}, []string{"agri"},
		"Koperasi 240 petani kopi dan padi di lereng Papandayan dan Cikuray. Mengoperasikan lelang mingguan kopi dan beras Jawa Barat.", true},
	{"Asosiasi UMKM Pangan Bandung Raya", "Asosiasi", "Bandung, Jawa Barat", "Asosiasi · Kemasan", "verified", []string{"packaging"}, []string{"packaging", "it"},
		"Wadah 300+ UMKM makanan dan kreatif Bandung Raya. Mengatur pengadaan kemasan bersama dan bursa talenta digital.", true},
	{"Gapoktan Sumber Rejeki Magelang", "Kelompok tani", "Magelang, Jawa Tengah", "Kelompok tani · Pertanian", "verified", []string{"agri", "food"}, []string{"agri"},
		"Gabungan 18 kelompok tani Magelang–Temanggung: pengadaan pupuk bersama dan pemasaran gula aren mitra.", true},
	{"PT Lintas Logistik Brantas", "PT", "Surabaya, Jawa Timur", "PT · Logistik", "verified", []string{"logistics"}, []string{"energy", "manufacturing"},
		"Operator logistik Jawa Timur yang juga menyelenggarakan market jasa: cold chain, PLTS UMKM Bali, dan jam mesin CNC.", true},
}

var demoBizOrgs = []demoOrgDef{
	{"Koperasi Tani Sumber Makmur Karawang", "Koperasi", "Karawang, Jawa Barat", "Koperasi · Pangan", "verified", []string{"food", "agri"}, []string{"agri", "packaging"},
		"Koperasi 120 petani padi Karawang dengan penggilingan sendiri. Menjual beras medium dan premium langsung ke ritel.", false},
	{"CV Berkah Kemasan Sidoarjo", "CV", "Sidoarjo, Jawa Timur", "CV · Kemasan", "verified", []string{"packaging"}, []string{"manufacturing"},
		"Produsen standing pouch, karung, dan paper bag cetak untuk UMKM makanan Jawa Timur.", false},
	{"PT Karya Karton Mandiri", "PT", "Tangerang, Banten", "PT · Kemasan", "verified", []string{"packaging", "manufacturing"}, []string{"manufacturing", "logistics"},
		"Pabrik box karton gelombang dan kertas kraft liner, kapasitas 1,5 juta box per bulan.", false},
	{"UMKM Keripik Ceu Imas Tasikmalaya", "UMKM", "Tasikmalaya, Jawa Barat", "UMKM · Pangan", "unverified", []string{"food"}, []string{"packaging", "agri"},
		"Keripik singkong dan kentang balado rumahan, dijual ke toko oleh-oleh di Priangan Timur.", false},
	{"PT Sangrai Kopi Kemang", "PT", "Jakarta Selatan, DKI Jakarta", "PT · Pangan", "verified", []string{"food"}, []string{"agri", "packaging", "logistics"},
		"Roastery specialty yang memasok 40 kafe di Jabodetabek.", false},
	{"CV Kedai Kopi Senja Bandung", "CV", "Bandung, Jawa Barat", "CV · Pangan", "pending", []string{"food"}, []string{"agri", "packaging"},
		"Jaringan tujuh kedai kopi di Bandung dan Cimahi.", false},
	{"PT Armada Andalan Logistik", "PT", "Bekasi, Jawa Barat", "PT · Logistik", "verified", []string{"logistics"}, []string{"energy", "manufacturing"},
		"Armada 45 truk engkel, CDD, dan fuso untuk Jabodetabek dan lintas Jawa–Sumatra.", false},
	{"CV Teknik Presisi Cikarang", "CV", "Cikarang, Kab. Bekasi, Jawa Barat", "CV · Manufaktur", "verified", []string{"manufacturing"}, []string{"manufacturing", "logistics"},
		"Bengkel CNC dan fabrikasi plat untuk vendor komponen otomotif di kawasan industri Cikarang.", false},
	{"PT Kode Nusa Digital", "PT", "Yogyakarta, DI Yogyakarta", "PT · Jasa IT", "verified", []string{"it"}, []string{"it"},
		"Studio software dan desain produk digital dengan 25 engineer di Yogyakarta.", false},
	{"Koperasi Nelayan Mina Bahari Sidoarjo", "Koperasi", "Sidoarjo, Jawa Timur", "Koperasi · Pangan", "unverified", []string{"food"}, []string{"logistics", "packaging"},
		"Koperasi nelayan dan pengolah ikan beku di pesisir Sidoarjo.", false},
	{"PT Surya Atap Dewata", "PT", "Denpasar, Bali", "PT · Energi", "verified", []string{"energy"}, []string{"manufacturing"},
		"Installer PLTS atap untuk vila, hotel kecil, dan UMKM di Bali.", false},
	{"Kelompok Tani Lereng Merapi Magelang", "Kelompok tani", "Magelang, Jawa Tengah", "Kelompok tani · Pertanian", "verified", []string{"agri"}, []string{"agri", "packaging"},
		"Kelompok tani cabai, bawang, dan sayur dataran tinggi di lereng Merapi.", false},
	{"UD Aren Lestari Toba", "UMKM", "Balige, Toba, Sumatera Utara", "UMKM · Pangan", "unverified", []string{"food", "energy"}, []string{"packaging"},
		"Pengrajin gula aren cetak, gula semut, dan briket tempurung di sekitar Danau Toba.", false},
	{"PT Swalayan Sejahtera Bekasi", "PT", "Bekasi, Jawa Barat", "PT · Pangan", "verified", []string{"food"}, []string{"food", "agri", "packaging", "logistics"},
		"Jaringan 60 minimarket di Bekasi dan Karawang.", false},
	{"CV Konveksi Sandang Laweyan", "CV", "Surakarta, Jawa Tengah", "CV · Manufaktur", "pending", []string{"manufacturing"}, []string{"manufacturing", "logistics"},
		"Konveksi seragam kerja dan sekolah di kampung batik Laweyan, Solo.", false},
}

type demoMarketDef struct {
	Maker                               int
	Name, Item, Region, Objective, Mech string
	Status                              string
	Demand, Supply, LotMin, LotMax, Ref float64
	CadenceDays, DurationDays           int
	Eligibility                         string
	Desc                                string
}

var demoMarkets = []demoMarketDef{
	{0, "Kopi Arabika Garut & Pangalengan", "Kopi arabika Java Preanger green bean", "Jawa Barat", "selling", "forward_auction", "active", 60_000, 45_000, 1_500, 5_000, 125_000, 7, 2, "verified",
		"Lelang mingguan green bean arabika Java Preanger dari petani Garut dan Pangalengan untuk roastery dan kafe."},
	{1, "Kemasan Kolektif UMKM Bandung", "Standing pouch zipper 250 g", "Jawa Barat", "procurement", "collective_procurement", "active", 840_000, 520_000, 60_000, 180_000, 1_450, 7, 3, "verified_docs",
		"Pengadaan kolektif kemasan untuk UMKM makanan Bandung Raya: volume digabung, harga pabrik."},
	{0, "Beras Medium Karawang", "Beras medium", "Jawa Barat", "selling", "forward_auction", "active", 650_000, 400_000, 10_000, 30_000, 13_000, 7, 2, "verified",
		"Koperasi tani Karawang dan Indramayu menjual beras medium langsung ke ritel modern dan distributor."},
	{1, "Box Karton E-commerce Jabodetabek", "Box karton double wall 40×30×20 cm", "Banten", "procurement", "reverse_auction", "active", 300_000, 180_000, 20_000, 60_000, 3_100, 7, 2, "verified_docs",
		"Seller online Jabodetabek membeli box karton standar bersama, pabrik karton bersaing harga."},
	{2, "Pupuk Organik Kolektif Jawa Tengah", "Pupuk organik granul", "Jawa Tengah", "procurement", "collective_procurement", "active", 1_200_000, 700_000, 60_000, 200_000, 2_000, 10, 3, "verified_docs",
		"Pengadaan pupuk organik bersama untuk kelompok tani Jawa Tengah, dikirim ke gudang gapoktan."},
	{1, "Talenta Engineer Yogyakarta", "Backend developer Go", "DI Yogyakarta", "service_exchange", "direct_market", "active", 1_240, 610, 80, 240, 210_000, 7, 2, "verified",
		"Jam kerja engineer backend untuk startup dan agensi di Yogyakarta."},
	{2, "Gula Aren Toba", "Gula aren cetak", "Sumatera Utara", "selling", "dutch_auction", "active", 90_000, 140_000, 2_000, 6_000, 29_000, 7, 1, "open",
		"Gula aren cetak dari pengrajin sekitar Danau Toba dilelang Dutch: harga turun bertahap sampai ada pembeli."},
	{3, "PLTS Atap UMKM Bali", "Paket PLTS atap 3 kWp on-grid", "Bali", "procurement", "sealed_bid", "active", 180, 40, 10, 30, 45_000_000, 14, 4, "verified_docs",
		"Paket PLTS atap untuk UMKM pariwisata Bali, penawaran tertutup sampai penutupan."},
	{3, "Cold Chain Surabaya–Malang", "Truk berpendingin Surabaya–Malang", "Jawa Timur", "service_exchange", "reverse_auction", "formation", 320, 210, 60, 120, 2_200_000, 7, 3, "verified_docs",
		"Trip truk berpendingin rutin untuk nelayan dan pengolah ikan Jawa Timur."},
	{3, "Jam Mesin CNC Bekasi–Karawang", "Jasa CNC machining", "Jawa Barat", "procurement", "reverse_auction", "paused", 2_400, 1_500, 120, 400, 230_000, 10, 2, "verified",
		"Jam mesin CNC untuk industri komponen di koridor Cikarang–Karawang."},
}

var demoCarriers = []string{"Armada sendiri", "Ekspedisi mitra", "Truk sewaan", "Kargo darat", "Kurir instan"}
