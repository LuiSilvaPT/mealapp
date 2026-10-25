// mealapp — a budget-aware meal builder over the live Glovo/Auchan catalogue.
//
//   go run .            serve on http://localhost:8080
//   go run . -port 9000
//   go run . -refresh   crawl Glovo now, then serve
//
// Endpoints:
//   GET  /                     the app (single embedded page)
//   GET  /api/products?q=&max=&limit=   matching products, cheapest first
//   GET  /api/stats            catalogue size + refresh progress
//   POST /api/refresh          re-crawl Glovo (runs in the background)
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var pageFS embed.FS

const (
	apiBase   = "https://api.glovoapp.com"
	storeID   = "118679" // Auchan, Castelo Branco
	storeAddr = "217450"
	lat       = "39.8222"
	lon       = "-7.4930"
	catalog   = "glovo_catalog.json"
)

type Product struct {
	Name     string   `json:"name"`
	Price    float64  `json:"price"`
	Unit     string   `json:"unit"`
	Weighted bool     `json:"weighted"`
	Section  string   `json:"section"`
	ExtID    string   `json:"extId"`
	ImageURL string   `json:"imageUrl"`
	Meals    []string `json:"meals"`
}

// ---- which meals a product belongs to ----------------------------------------

var sectionMeals = map[string][]string{
	// breakfast
	"Cereais e Barras":                     {"breakfast", "snack"},
	"Iogurtes Sólidos":                     {"breakfast", "snack"},
	"Iogurtes Líquidos":                    {"breakfast", "snack"},
	"Alternativas ao Iogurte":              {"breakfast", "snack"},
	"Bebidas Lácteas":                      {"breakfast"},
	"Chá, Achocolatados e Bebidas Cereais": {"breakfast"},
	"Café":                                 {"breakfast"},
	"Pão":                                  {"breakfast", "lunch", "dinner"},
	"Tostas, Gressinos e Croutons":         {"breakfast", "snack"},
	"Queijos":                              {"breakfast", "lunch", "dinner"},
	"Charcutaria":                          {"breakfast", "lunch", "dinner"},
	"Compotas, Cremes e Mel":               {"breakfast"},
	"Manteiga e Cremes Culinários":         {"breakfast", "lunch", "dinner"},
	"Croissants, Folhados e Brioches":      {"breakfast", "snack"},
	"Pastelaria":                           {"breakfast", "snack"},
	"Ovos":                                 {"breakfast", "lunch", "dinner"},
	"Fruta Fresca":                         {"breakfast", "snack"},
	// savoury
	"Arroz, Massa e Farinha":          {"lunch", "dinner"},
	"Massas e Noodles":                {"lunch", "dinner"},
	"Massas para Culinária":           {"lunch", "dinner"},
	"Batatas e Purés":                 {"lunch", "dinner"},
	"Legumes Frescos":                 {"lunch", "dinner"},
	"Frutas e Legumes":                {"lunch", "dinner"},
	"Conservas":                       {"lunch", "dinner"},
	"Leguminosas Secas":               {"lunch", "dinner"},
	"Sal e Temperos":                  {"lunch", "dinner"},
	"Azeite, Óleo e Vinagre":          {"lunch", "dinner"},
	"Ketchup e Molhos":                {"lunch", "dinner"},
	"Natas e Bechamel":                {"lunch", "dinner"},
	"Sopas e Saladas":                 {"dinner", "lunch"},
	"Salada, Preparados e Salteados":  {"lunch", "dinner"},
	"Refeições Frescas":               {"lunch", "dinner"},
	"Refeições Congeladas":            {"lunch", "dinner"},
	"Peixe e Marisco Congelados":      {"lunch", "dinner"},
	"Talho Congelado":                 {"lunch", "dinner"},
	"Pizzas":                          {"lunch", "dinner"},
	"Hambúrgueres e Sandes":           {"lunch", "dinner"},
	"Tábuas":                          {"lunch", "dinner"},
	"Sabores do Mundo":                {"lunch", "dinner"},
	"Salgados e Pastelaria":           {"snack", "lunch"},
	"Opções Vegetarianas":             {"lunch", "dinner"},
	"Vegetariano e Vegan":             {"lunch", "dinner"},
	"Vegan e Veggie":                  {"lunch", "dinner"},
	"Biológicos":                      {"breakfast", "lunch", "dinner", "snack"},
	"Sem Glúten":                      {"breakfast", "lunch", "dinner", "snack"},
	"Sem Lactose":                     {"breakfast", "lunch", "dinner", "snack"},
	"Aves e Coelho":                   {"lunch", "dinner"},
	"Bovino":                          {"lunch", "dinner"},
	"Porco":                           {"lunch", "dinner"},
	"Mais Carnes":                     {"lunch", "dinner"},
	"Bacalhau":                        {"lunch", "dinner"},
	"Peixe Fresco":                    {"lunch", "dinner"},
	"Filetes, Lombos e Postas":        {"lunch", "dinner"},
	"Marisco":                         {"lunch", "dinner"},
	"Salmão Fumado e Caviar":          {"breakfast", "lunch"},
	"Salmão Fumado e Especialidades":  {"breakfast", "lunch"},
	// snacks
	"Bolachas e Biscoitos":                {"snack", "breakfast"},
	"Biscoitos Tradicionais e Bolachas":   {"snack", "breakfast"},
	"Batatas Fritas e Snacks":             {"snack"},
	"Frutos Secos, Desidratados e Sementes": {"snack", "breakfast"},
	"Gelados e Tartes Geladas":            {"snack"},
	"Sobremesas Congeladas":               {"snack"},
	"Gomas, Pastilhas e Rebuçados":        {"snack"},
	"Chocolates":                          {"snack"},
	"Bolos e Sobremesas":                  {"snack"},
	"Sobremesas":                          {"snack"},
	"Aperitivos e Salgados":               {"snack"},
	"Nutrição Desportiva":                 {"snack"},
	"Bebidas Energéticas e Isotónicas":    {"snack"},
	"Refrigerantes":                       {"snack"},
	"Água":                                {"snack"},
	"Gelo":                                {"snack"},
}

var mealKeywords = map[string][]string{
	"breakfast": {"leite", "iogurte", "cereal", "muesli", "granola", "aveia", "torrada",
		"tosta", "pão", "café", "marmelada", "compota", "mel", "manteiga", "queijo",
		"fiambre", "croissant", "brioche", "ovo", "chá", "cacau", "bolacha maria"},
	"lunch": {"arroz", "massa", "esparguete", "penne", "frango", "carne", "bife",
		"bacalhau", "atum", "sardinha", "feijão", "grão", "batata", "legumes",
		"salsicha", "chouriço", "porco", "peru", "salmão"},
	"dinner": {"sopa", "caldo", "creme de legumes", "arroz", "massa", "bacalhau",
		"atum", "sardinha", "feijão", "frango", "batata", "legumes", "peixe"},
	"snack": {"bolacha", "gelado", "chocolate", "gomas", "batata frita", "frutos secos",
		"amendoim", "barra", "biscoito"},
}

func classify(p Product) []string {
	set := map[string]bool{}
	for _, m := range sectionMeals[p.Section] {
		set[m] = true
	}
	padded := words(p.Name)
	for meal, kws := range mealKeywords {
		if hasAnyWord(padded, kws) {
			set[meal] = true
		}
	}
	out := make([]string, 0, 4)
	for _, m := range []string{"breakfast", "lunch", "dinner", "snack"} {
		if set[m] {
			out = append(out, m)
		}
	}
	return out
}

// ---- pantry staples: things you probably already have -------------------------

type stapleDef struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Find      []string `json:"-"`
	In        []string `json:"-"`
	ImpliedBy []string `json:"impliedBy"`
}

var stapleDefs = []stapleDef{
	{"sal", "Salt", []string{"sal"}, []string{"Sal e Temperos"}, []string{"Arroz, Massa e Farinha", "Legumes Frescos", "Ovos", "Aves e Coelho", "Porco", "Bovino", "Bacalhau", "Peixe Fresco", "Massas e Noodles"}},
	{"azeite", "Olive oil", []string{"azeite"}, []string{"Azeite, Óleo e Vinagre"}, []string{"Arroz, Massa e Farinha", "Legumes Frescos", "Aves e Coelho", "Porco", "Bovino", "Bacalhau", "Peixe Fresco", "Massas e Noodles", "Batatas e Purés"}},
	{"alho", "Garlic", []string{"alho"}, []string{"Sal e Temperos", "Legumes Frescos"}, []string{"Aves e Coelho", "Porco", "Bovino", "Legumes Frescos", "Arroz, Massa e Farinha"}},
	{"cebola", "Onion", []string{"cebola"}, []string{"Legumes Frescos"}, []string{"Aves e Coelho", "Porco", "Bovino", "Legumes Frescos", "Arroz, Massa e Farinha"}},
	{"pimenta", "Pepper", []string{"pimenta"}, []string{"Sal e Temperos"}, []string{"Aves e Coelho", "Porco", "Bovino", "Bacalhau", "Peixe Fresco"}},
	{"farinha", "Flour", []string{"farinha"}, []string{"Arroz, Massa e Farinha"}, []string{"Batatas e Purés", "Sopas e Saladas"}},
	{"acucar", "Sugar", []string{"acucar"}, []string{"Açúcar e Sobremesas", "Arroz, Massa e Farinha"}, []string{"Café", "Cereais e Barras"}},
	{"vinagre", "Vinegar", []string{"vinagre"}, []string{"Azeite, Óleo e Vinagre"}, []string{"Legumes Frescos", "Salada, Preparados e Salteados"}},
	{"louro", "Bay leaf", []string{"louro"}, []string{"Sal e Temperos"}, []string{"Sopas e Saladas", "Aves e Coelho"}},
	{"oregaos", "Oregano", []string{"oregaos", "oregãos"}, []string{"Sal e Temperos"}, []string{"Pizzas", "Massas e Noodles"}},
}

func handleStaples(w http.ResponseWriter, r *http.Request) {
	type offer struct {
		Name  string  `json:"name"`
		Price float64 `json:"price"`
	}
	type out struct {
		Key       string   `json:"key"`
		Label     string   `json:"label"`
		ImpliedBy []string `json:"impliedBy"`
		Product   *offer   `json:"product"`
	}
	mu.RLock()
	defer mu.RUnlock()
	res := make([]out, 0, len(stapleDefs))
	for _, d := range stapleDefs {
		o := out{Key: d.Key, Label: d.Label, ImpliedBy: d.ImpliedBy}
		best := -1
		for i, p := range products {
			if len(d.In) > 0 {
				ok := false
				for _, sec := range d.In {
					if p.Section == sec {
						ok = true
						break
					}
				}
				if !ok {
					continue
				}
			}
			if !hasAnyWord(words(p.Name), d.Find) {
				continue
			}
			if best < 0 || p.Price < products[best].Price {
				best = i
			}
		}
		if best >= 0 {
			o.Product = &offer{products[best].Name, products[best].Price}
		}
		res = append(res, o)
	}
	writeJSON(w, map[string]any{"staples": res})
}

// ---- tile schema of the Glovo content API -------------------------------------

type tile struct {
	Type string `json:"type"`
	Data struct {
		Name       string  `json:"name"`
		Price      float64 `json:"price"`
		ImageURL   string  `json:"imageUrl"`
		ExternalID string  `json:"externalId"`
		Title      string  `json:"title"`
		Slug       string  `json:"slug"`
		ContentURI string  `json:"contentUri"`
		Tracking   struct {
			SaleType string `json:"productSaleType"`
			Weighted bool   `json:"isWeightedProduct"`
		} `json:"tracking"`
		Action struct {
			Data struct {
				Path string `json:"path"`
			} `json:"data"`
		} `json:"action"`
		Elements []tile `json:"elements"`
		Body     []tile `json:"body"`
	} `json:"data"`
}

type layout struct {
	Data struct {
		Body []tile `json:"body"`
	} `json:"data"`
}

var foodCategories = []string{
	"Frutas e Vegetais", "Talho e Peixaria", "Charcutaria e Queijos", "Padaria e Pastelaria",
	"Laticínios e Ovos", "Mercearia", "Mercearia Doce", "Refeições Prontas", "Gelados e Congelados",
	"Águas e Sumos", "Bio e Saudável",
}

// ---- in-memory catalogue ------------------------------------------------------

var (
	mu       sync.RWMutex
	products []Product
	updated  time.Time
	search   []string // folded names, parallel to products
)

func setProducts(p []Product) {
	folded := make([]string, len(p))
	for i := range p {
		p[i].Meals = classify(p[i])
		folded[i] = fold(p[i].Name + " " + p[i].Section)
	}
	mu.Lock()
	products, search, updated = p, folded, time.Now()
	mu.Unlock()
}

func loadCatalog() error {
	b, err := os.ReadFile(catalog)
	if err != nil {
		return err
	}
	var p []Product
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	setProducts(p)
	return nil
}

func saveCatalog(p []Product) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(catalog, b, 0o644)
}

// ---- crawling Glovo -----------------------------------------------------------

var client = &http.Client{Timeout: 30 * time.Second}

func glovoGet(path string) (*layout, error) {
	var last error
	for attempt := 0; attempt < 6; attempt++ {
		time.Sleep(120 * time.Millisecond)
		req, err := http.NewRequest("GET", apiBase+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Glovo-Api-Version", "4")
		req.Header.Set("Glovo-Language-Code", "pt")
		req.Header.Set("Glovo-App-Platform", "web")
		req.Header.Set("Glovo-Delivery-Location-Latitude", lat)
		req.Header.Set("Glovo-Delivery-Location-Longitude", lon)
		req.Header.Set("Glovo-Delivery-Location-Accuracy", "15")
		req.Header.Set("Glovo-Delivery-Location-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
		req.Header.Set("Glovo-Location-Country-Code", "PT")
		req.Header.Set("Origin", "https://glovoapp.com")
		req.Header.Set("Referer", "https://glovoapp.com/")

		resp, err := client.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			var l layout
			if err := json.Unmarshal(body, &l); err != nil {
				return nil, err
			}
			return &l, nil
		}
		if resp.StatusCode == 429 || resp.StatusCode == 503 {
			time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
			last = fmt.Errorf("status %d", resp.StatusCode)
			continue
		}
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil, last
}

type crawlState struct {
	Running bool      `json:"running"`
	Count   int       `json:"count"`
	Started time.Time `json:"started"`
	Error   string    `json:"error"`
}

var (
	crawlMu    sync.Mutex
	crawl      crawlState
	refreshing bool
)

func crawlAll() ([]Product, error) {
	main, err := glovoGet(fmt.Sprintf("/v4/stores/%s/addresses/%s/content/main", storeID, storeAddr))
	if err != nil {
		return nil, fmt.Errorf("store front page: %w", err)
	}

	type job struct{ url, section string }
	var queue []job
	wanted := map[string]bool{}
	for _, c := range foodCategories {
		wanted[c] = true
	}
	for _, sec := range main.Data.Body {
		// On the front page action.data.path is only the category's *first*
		// sub-collection, so seed from the slug, which returns the whole category.
		if !wanted[sec.Data.Title] || sec.Data.Slug == "" {
			continue
		}
		seed := fmt.Sprintf("/v4/stores/%s/addresses/%s/content/main?nodeType=DEEP_LINK&link=%s",
			storeID, storeAddr, sec.Data.Slug)
		queue = append(queue, job{seed, sec.Data.Title})
	}

	var out []Product
	seen := map[string]bool{}
	visited := map[string]bool{}

	var walk func(t tile, section string)
	walk = func(t tile, section string) {
		switch t.Type {
		case "PRODUCT_TILE":
			if t.Data.Name == "" {
				return
			}
			key := t.Data.ExternalID + "|" + t.Data.Name
			if seen[key] {
				return
			}
			seen[key] = true
			out = append(out, Product{
				Name:     t.Data.Name,
				Price:    t.Data.Price,
				Unit:     t.Data.Tracking.SaleType,
				Weighted: t.Data.Tracking.Weighted,
				Section:  section,
				ExtID:    t.Data.ExternalID,
				ImageURL: t.Data.ImageURL,
			})
			return
		case "COLLECTION_TILE":
			if p := t.Data.Action.Data.Path; p != "" {
				title := t.Data.Title
				if title == "" {
					title = section
				}
				queue = append(queue, job{p, title})
			}
			return
		case "CONTENT_PLACEHOLDER":
			if t.Data.ContentURI != "" {
				queue = append(queue, job{t.Data.ContentURI, section})
			}
			return
		}
		for _, e := range t.Data.Elements {
			walk(e, section)
		}
		for _, b := range t.Data.Body {
			walk(b, section)
		}
	}

	for len(queue) > 0 {
		j := queue[0]
		queue = queue[1:]
		if visited[j.url] {
			continue
		}
		visited[j.url] = true
		l, err := glovoGet(j.url)
		if err != nil {
			continue
		}
		for _, b := range l.Data.Body {
			walk(b, j.section)
		}
		crawlMu.Lock()
		crawl.Count = len(out)
		crawlMu.Unlock()
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("crawl returned nothing")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	return out, nil
}

func refresh() {
	crawlMu.Lock()
	if refreshing {
		crawlMu.Unlock()
		return
	}
	refreshing = true
	crawl = crawlState{Running: true, Started: time.Now()}
	crawlMu.Unlock()

	p, err := crawlAll()
	if err == nil {
		err = saveCatalog(p)
	}
	if err == nil {
		setProducts(p)
	}

	crawlMu.Lock()
	crawl.Running = false
	if err != nil {
		crawl.Error = err.Error()
	} else {
		crawl.Count = len(p)
	}
	refreshing = false
	crawlMu.Unlock()
}

// ---- helpers ------------------------------------------------------------------

var accent = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a",
	"é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i",
	"ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u",
	"ç", "c", "ñ", "n",
)

func fold(s string) string { return accent.Replace(strings.ToLower(s)) }

// words turns a string into a space-padded run of lowercase words, so keyword
// matching can't hit substrings ("mel" must not match "Mélias").
func words(s string) string {
	var b strings.Builder
	b.WriteByte(' ')
	for _, r := range fold(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	b.WriteByte(' ')
	return b.String()
}

func hasWord(padded, w string) bool { return strings.Contains(padded, " "+w+" ") }

func hasAnyWord(padded string, ws []string) bool {
	for _, w := range ws {
		if hasWord(padded, w) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- handlers -----------------------------------------------------------------

func handleProducts(w http.ResponseWriter, r *http.Request) {
	q := fold(strings.TrimSpace(r.URL.Query().Get("q")))
	section := r.URL.Query().Get("section")
	meal := r.URL.Query().Get("meal")
	maxPrice, _ := strconv.ParseFloat(r.URL.Query().Get("max"), 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 48
	}
	terms := strings.Fields(q)

	mu.RLock()
	out := make([]Product, 0, limit)
	matched, skipped := 0, 0
	for i, p := range products {
		if section != "" && p.Section != section {
			continue
		}
		if meal != "" {
			found := false
			for _, m := range p.Meals {
				if m == meal {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if maxPrice > 0 && p.Price > maxPrice {
			continue
		}
		if len(terms) > 0 {
			hay := search[i]
			ok := true
			for _, t := range terms {
				if !strings.Contains(hay, t) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		matched++
		if skipped < offset {
			skipped++
			continue
		}
		if len(out) >= limit {
			continue
		}
		out = append(out, p)
	}
	total := len(products)
	mu.RUnlock()

	writeJSON(w, map[string]any{
		"total": total, "matched": matched, "offset": offset,
		"count": len(out), "products": out,
	})
}

func handleSections(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	counts := map[string]int{}
	for _, p := range products {
		counts[p.Section]++
	}
	mu.RUnlock()
	type sec struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := make([]sec, 0, len(counts))
	for n, c := range counts {
		out = append(out, sec{n, c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, map[string]any{"sections": out})
}

func handleStats(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	n, at := len(products), updated
	mu.RUnlock()
	crawlMu.Lock()
	c := crawl
	crawlMu.Unlock()
	writeJSON(w, map[string]any{"products": n, "updated": at, "refresh": c})
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	go refresh()
	writeJSON(w, map[string]any{"started": true})
}

func main() {
	port := flag.Int("port", 8080, "port to listen on")
	host := flag.String("host", "0.0.0.0", "address to bind")
	doRefresh := flag.Bool("refresh", false, "crawl Glovo on startup")
	flag.Parse()

	if err := loadCatalog(); err != nil {
		log.Printf("no local catalogue yet (%v) — starting a crawl", err)
		p, cerr := crawlAll()
		if cerr != nil {
			log.Fatalf("crawl failed: %v", cerr)
		}
		_ = saveCatalog(p)
		setProducts(p)
	}
	mu.RLock()
	n := len(products)
	mu.RUnlock()
	log.Printf("loaded %d products", n)

	if *doRefresh {
		go refresh()
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := pageFS.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	http.HandleFunc("/api/products", handleProducts)
	http.HandleFunc("/api/sections", handleSections)
	http.HandleFunc("/api/staples", handleStaples)
	http.HandleFunc("/api/stats", handleStats)
	http.HandleFunc("/api/refresh", handleRefresh)

	addr := *host + ":" + strconv.Itoa(*port)
	log.Printf("meal builder on http://localhost:%d  (%s)", *port, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// keep net/url imported for future query building without churn
var _ = url.Values{}
