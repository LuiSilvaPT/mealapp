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
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"html"
	"image/jpeg"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html hero.jpg
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
	Name      string   `json:"name"`
	Price     float64  `json:"price"`
	Unit      string   `json:"unit"`
	Weighted  bool     `json:"weighted"`
	Section   string   `json:"section"`
	ExtID     string   `json:"extId"`
	ImageURL  string   `json:"imageUrl"`
	Meals     []string `json:"meals"`
	Role      string   `json:"role"`
	UnitPrice float64  `json:"unitPrice"`
	UnitLabel string   `json:"unitLabel"`
}

// ---- food roles, used by the auto-fill and the balance warning -----------------

var sectionRole = map[string]string{
	"Aves e Coelho": "protein", "Bovino": "protein", "Porco": "protein",
	"Mais Carnes": "protein", "Bacalhau": "protein", "Peixe Fresco": "protein",
	"Filetes, Lombos e Postas": "protein", "Marisco": "protein",
	"Talho Congelado": "protein", "Peixe e Marisco Congelados": "protein",
	"Salgados e Pastelaria": "protein", "Conservas": "protein", "Ovos": "protein",
	"Leguminosas Secas": "protein", "Charcutaria": "protein",
	"Salmão Fumado e Caviar": "protein", "Salmão Fumado e Especialidades": "protein",
	"Refeições Frescas": "protein", "Refeições Congeladas": "protein",

	"Arroz, Massa e Farinha": "carb", "Massas e Noodles": "carb",
	"Massas para Culinária": "carb", "Batatas e Purés": "carb",
	"Pão": "carb", "Tostas, Gressinos e Croutons": "carb",
	"Cereais e Barras": "carb", "Pizzas": "carb", "Hambúrgueres e Sandes": "carb",

	"Legumes Frescos": "veg", "Frutas e Legumes": "veg",
	"Salada, Preparados e Salteados": "veg", "Sopas e Saladas": "veg",

	"Fruta Fresca": "fruit",
	"Frutos Secos, Desidratados e Sementes": "fruit",

	"Queijos": "dairy", "Iogurtes Sólidos": "dairy", "Iogurtes Líquidos": "dairy",
	"Alternativas ao Iogurte": "dairy", "Bebidas Lácteas": "dairy",
	"Manteiga e Cremes Culinários": "dairy", "Natas e Bechamel": "dairy",
	"Sem Lactose": "dairy", "Sem Glúten": "dairy",
}

func role(p Product) string {
	if r, ok := sectionRole[p.Section]; ok {
		return r
	}
	return "other"
}

// ---- normalised price: what does it cost per kg or per litre? ------------------

var (
	sizeRe   = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*\)?\s*(kg|quilograma|g|gr|gramas|grama|ml|cl|lt|l|litro|litros)\b`)
	multRe   = regexp.MustCompile(`(?i)(\d+)\s*[x×]\s*(\d+(?:[.,]\d+)?)\s*(ml|cl|lt|l|g|kg)`)
	numToF64 = func(s string) float64 {
		v, _ := strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64)
		return v
	}
)

// parseSize reads a size out of a product name the way Portuguese writes them:
// "1.200 GR" is 1200 g, and "0.375G" is 375 g, not 0.375 g. Reading those
// literally produced unit prices off by a factor of a thousand.
func parseSize(raw, unit string) float64 {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, ".") && !strings.Contains(raw, ",") {
		if parts := strings.Split(raw, "."); len(parts) == 2 && len(parts[1]) == 3 {
			raw = parts[0] + parts[1]
		}
	}
	v := numToF64(raw)
	switch strings.ToLower(unit) {
	case "g", "gr", "grama", "gramas":
		if v > 0 && v < 1 {
			v *= 1000
		}
	}
	return v
}

// unitPrice returns the price per kg (for solids) or per litre (for liquids).
func unitPrice(p Product) (float64, string) {
	if p.Weighted {
		return p.Price, "kg"
	}
	amount, label := 0.0, ""
	if m := multRe.FindStringSubmatch(p.Name); m != nil {
		amount = parseSize(m[1], m[3]) * numToF64(m[2])
		label = strings.ToLower(m[3])
	} else if m := sizeRe.FindStringSubmatch(p.Name); m != nil {
		amount = parseSize(m[1], m[2])
		label = strings.ToLower(m[2])
	}
	if amount <= 0 {
		return 0, ""
	}
	switch label {
	case "kg", "quilograma":
		return p.Price / amount, "kg"
	case "g", "gr", "grama", "gramas":
		return p.Price / amount * 1000, "kg"
	case "l", "lt", "litro", "litros":
		return p.Price / amount, "L"
	case "ml":
		return p.Price / amount * 1000, "L"
	case "cl":
		return p.Price / (amount / 100), "L"
	}
	return 0, ""
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
	Triggers  []string `json:"triggers"`
}

// Triggers are the words in a product name that mean "this dish needs it".
var stapleDefs = []stapleDef{
	{"sal", "Salt", []string{"sal"}, []string{"Sal e Temperos", "Sal, Ervas e Temperos"},
		[]string{"Arroz, Massa e Farinha", "Legumes Frescos", "Ovos", "Aves e Coelho", "Porco", "Bovino", "Bacalhau", "Peixe Fresco", "Massas e Noodles"},
		[]string{"batata", "arroz", "massa", "esparguete", "penne", "frango", "carne", "bife", "porco", "peixe", "bacalhau", "atum", "ovo", "legumes", "feijao", "grao", "sopa", "caldo", "cenoura", "tomate", "salada", "pure", "risotto", "couve", "grelos"}},
	{"azeite", "Olive oil", []string{"azeite"}, []string{"Azeite, Óleo e Vinagre"},
		[]string{"Arroz, Massa e Farinha", "Legumes Frescos", "Aves e Coelho", "Porco", "Bovino", "Bacalhau", "Peixe Fresco", "Massas e Noodles", "Batatas e Purés"},
		[]string{"batata", "arroz", "massa", "frango", "carne", "peixe", "bacalhau", "legumes", "salada", "tomate", "ovo", "sopa"}},
	{"alho", "Garlic", []string{"alho"}, []string{"Sal e Temperos", "Legumes Frescos", "Sal, Ervas e Temperos", "Batatas, Alho e Cebola"},
		[]string{"Aves e Coelho", "Porco", "Bovino", "Legumes Frescos", "Arroz, Massa e Farinha"},
		[]string{"frango", "carne", "arroz", "massa", "batata", "legumes", "peixe", "bacalhau"}},
	{"cebola", "Onion", []string{"cebola"}, []string{"Legumes Frescos", "Batatas, Alho e Cebola"},
		[]string{"Aves e Coelho", "Porco", "Bovino", "Legumes Frescos", "Arroz, Massa e Farinha"},
		[]string{"frango", "carne", "arroz", "sopa", "feijao", "tomate", "batata", "bacalhau"}},
	{"pimenta", "Pepper", []string{"pimenta"}, []string{"Sal e Temperos", "Sal, Ervas e Temperos"},
		[]string{"Aves e Coelho", "Porco", "Bovino", "Bacalhau", "Peixe Fresco"},
		[]string{"frango", "carne", "peixe", "bacalhau", "ovo", "bife"}},
	{"farinha", "Flour", []string{"farinha"}, []string{"Arroz, Massa e Farinha", "Farinha"},
		[]string{"Batatas e Purés", "Sopas e Saladas"},
		[]string{"pure", "sopa", "molho", "carne"}},
	{"acucar", "Sugar", []string{"acucar"}, []string{"Açúcar e Sobremesas", "Arroz, Massa e Farinha", "Açúcar e Adoçante"},
		[]string{"Café", "Cereais e Barras"},
		[]string{"cafe", "cereais", "muesli", "cha", "bolo"}},
	{"vinagre", "Vinegar", []string{"vinagre"}, []string{"Azeite, Óleo e Vinagre"},
		[]string{"Legumes Frescos", "Salada, Preparados e Salteados"},
		[]string{"salada", "legumes", "alface"}},
	{"louro", "Bay leaf", []string{"louro"}, []string{"Sal e Temperos", "Sal, Ervas e Temperos"},
		[]string{"Sopas e Saladas", "Aves e Coelho"},
		[]string{"sopa", "feijao", "frango", "carne"}},
	{"oregaos", "Oregano", []string{"oregaos", "oregãos"}, []string{"Sal e Temperos", "Sal, Ervas e Temperos"},
		[]string{"Pizzas", "Massas e Noodles"},
		[]string{"pizza", "massa", "tomate"}},
}

func handleStaples(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	list := products
	mu.RUnlock()
	writeJSON(w, map[string]any{"staples": stapleOffers(list)})
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
		p[i].Role = role(p[i])
		p[i].UnitPrice, p[i].UnitLabel = unitPrice(p[i])
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

func numOrZero(s string) float64 {
	v, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(s), ",", ".", 1), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

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

// ---- Auchan direct: their own store, food only ------------------------------

const (
	auchanCatalog = "auchan_catalog.json"
	auchanGrid    = "https://www.auchan.pt/on/demandware.store/Sites-AuchanPT-Site/pt_PT/Search-UpdateGrid"
	auchanCGIDs   = "alimentacao-,produtos-frescos"
	browserUA     = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)

var (
	aMu       sync.RWMutex
	aProducts []Product
	aSearch   []string
	aUpdated  time.Time
)

// Auchan names its grocery sections differently, so they get their own maps.
var auchanSectionMeals = map[string][]string{
	"Cereais e Barras":            {"breakfast", "snack"},
	"Iogurtes":                    {"breakfast", "snack"},
	"Leites":                      {"breakfast"},
	"Bebidas Vegetais":            {"breakfast"},
	"Bebidas de Café Refrigeradas": {"breakfast"},
	"Café, Chá e Infusão":         {"breakfast"},
	"Ice Tea e Tisanas":           {"breakfast"},
	"Chocolates e Achocolatados":  {"breakfast", "snack"},
	"Cremes, Compotas e Mel":      {"breakfast"},
	"Manteiga, Cremes e Margarina": {"breakfast", "lunch", "dinner"},
	"Tostas e Pão Embalado":       {"breakfast", "lunch", "dinner"},
	"Bolachas e Bolos":            {"breakfast", "snack"},
	"Ovos":                        {"breakfast", "lunch", "dinner"},
	"Açúcar e Adoçante":           {"breakfast", "lunch", "dinner"},
	"Arroz e Massa":               {"lunch", "dinner"},
	"Farinha":                     {"lunch", "dinner"},
	"Conservas":                   {"lunch", "dinner"},
	"Azeite, Óleo e Vinagre":      {"lunch", "dinner"},
	"Sal, Ervas e Temperos":       {"lunch", "dinner"},
	"Polpas, Caldos e Temperos":   {"lunch", "dinner"},
	"Maionese, Ketchup, Mostarda e Especialidades": {"lunch", "dinner"},
	"Natas, Béchamel e Chantilly": {"lunch", "dinner"},
	"Leite Condensado e Preparado para Bolos": {"lunch", "dinner"},
	"Refeições, Sopas e Puré":     {"lunch", "dinner"},
	"Proteína":                    {"lunch", "dinner"},
	"Preparação de Refeições":     {"lunch", "dinner"},
	"Batatas Fritas e Aperitivos Snacks": {"snack"},
	"Pastilhas, Rebuçados e Chupas": {"snack"},
	"Gelatinas e Sobremesas":      {"snack"},
	"Sobremesas":                  {"snack"},
	// fresh aisles
	"Pão Fresco e Broa":            {"breakfast", "lunch", "dinner"},
	"Pastelaria Tradicional":       {"breakfast", "snack"},
	"Pastelaria Regional":          {"breakfast", "snack"},
	"Croissants, Folhados e Brioches": {"breakfast", "snack"},
	"Bolos Embalados":              {"breakfast", "snack"},
	"Bolos de Aniversário":         {"snack"},
	"Biscoitos Tradicionais e Bolachas": {"breakfast", "snack"},
	"Banana e Frutos Tropicais":    {"breakfast", "snack"},
	"Maçãs, Peras e Uvas":          {"breakfast", "snack"},
	"Fruta da Época":               {"breakfast", "snack"},
	"Frutos Secos e Sementes":      {"breakfast", "snack"},
	"Sumos de Fruta":               {"breakfast", "snack"},
	"Peixe Fresco":                 {"lunch", "dinner"},
	"Postas de Peixe":              {"lunch", "dinner"},
	"Bacalhau":                     {"lunch", "dinner"},
	"Marisco":                      {"lunch", "dinner"},
	"Polvo, Lulas e Choco":         {"lunch", "dinner"},
	"Camarão e Miolo de Camarão":   {"lunch", "dinner"},
	"Salmão Fumado, Ovas e Preparados": {"breakfast", "lunch"},
	"Porco":                        {"lunch", "dinner"},
	"Vitela e Novilho":             {"lunch", "dinner"},
	"Frango e Galinha":             {"lunch", "dinner"},
	"Hambúrgueres e Carne Picada":  {"lunch", "dinner"},
	"Chouriço, Bacon e Outros":     {"lunch", "dinner"},
	"Patés, Salsichas e Mortadela": {"lunch", "dinner"},
	"Alheira, Farinheira e Morcela": {"lunch", "dinner"},
	"Elaborados":                   {"lunch", "dinner"},
	"Presunto":                     {"breakfast", "lunch"},
	"Mais Legumes":                 {"lunch", "dinner"},
	"Tomate, Pepino e Pimentos":    {"lunch", "dinner"},
	"Batatas, Alho e Cebola":       {"lunch", "dinner"},
	"Cogumelos":                    {"lunch", "dinner"},
	"Saladas Embaladas e Preparados para Sopas": {"lunch", "dinner"},
	"Sopas e Preparados para Sopa": {"lunch", "dinner"},
	"Refeições Prontas":            {"lunch", "dinner"},
	"Massas Frescas":               {"lunch", "dinner"},
	"Ervas Aromáticas":             {"lunch", "dinner"},
}

func auchanMeals(section string) []string {
	if m, ok := auchanSectionMeals[section]; ok {
		return m
	}
	switch {
	case strings.HasPrefix(section, "Cozinha "), section == "Halal":
		return []string{"lunch", "dinner"}
	case strings.HasPrefix(section, "Queijo"), section == "Queijaria":
		return []string{"breakfast", "lunch", "dinner"}
	case strings.HasPrefix(section, "Biológicos"), section == "Sem Lactose":
		return []string{"breakfast", "lunch", "dinner", "snack"}
	}
	return nil
}

var auchanSectionRole = map[string]string{
	"Conservas": "protein", "Ovos": "protein", "Proteína": "protein",
	"Arroz e Massa": "carb", "Farinha": "carb", "Tostas e Pão Embalado": "carb",
	"Cereais e Barras": "carb",
	"Leites": "dairy", "Iogurtes": "dairy", "Manteiga, Cremes e Margarina": "dairy",
	"Natas, Béchamel e Chantilly": "dairy", "Bebidas Vegetais": "dairy",
	"Leite Sem Lactose": "dairy", "Leite Condensado e Preparado para Bolos": "dairy",
	"Biológicos Lácteos": "dairy",
	"Peixe Fresco": "protein", "Postas de Peixe": "protein", "Bacalhau": "protein",
	"Marisco": "protein", "Polvo, Lulas e Choco": "protein",
	"Camarão e Miolo de Camarão": "protein", "Salmão Fumado, Ovas e Preparados": "protein",
	"Porco": "protein", "Vitela e Novilho": "protein", "Frango e Galinha": "protein",
	"Hambúrgueres e Carne Picada": "protein", "Chouriço, Bacon e Outros": "protein",
	"Patés, Salsichas e Mortadela": "protein", "Alheira, Farinheira e Morcela": "protein",
	"Elaborados": "protein", "Presunto": "protein", "Refeições Prontas": "protein",
	"Mais Legumes": "veg", "Tomate, Pepino e Pimentos": "veg",
	"Batatas, Alho e Cebola": "veg", "Cogumelos": "veg",
	"Saladas Embaladas e Preparados para Sopas": "veg", "Ervas Aromáticas": "veg",
	"Banana e Frutos Tropicais": "fruit", "Maçãs, Peras e Uvas": "fruit",
	"Fruta da Época": "fruit", "Frutos Secos e Sementes": "fruit",
	"Pão Fresco e Broa": "carb", "Pastelaria Tradicional": "carb",
	"Pastelaria Regional": "carb", "Croissants, Folhados e Brioches": "carb",
	"Bolos Embalados": "carb", "Biscoitos Tradicionais e Bolachas": "carb",
	"Massas Frescas": "carb",
}

func roleAuchan(section string) string {
	if r, ok := auchanSectionRole[section]; ok {
		return r
	}
	switch {
	case strings.HasPrefix(section, "Queijo"), section == "Queijaria":
		return "dairy"
	case strings.HasPrefix(section, "Cozinha "), section == "Halal":
		return "protein"
	}
	return "other"
}

func classifyAuchan(p Product) []string {
	set := map[string]bool{}
	for _, m := range auchanMeals(p.Section) {
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

func setAuchan(p []Product) {
	folded := make([]string, len(p))
	for i := range p {
		p[i].Meals = classifyAuchan(p[i])
		p[i].Role = roleAuchan(p[i].Section)
		p[i].UnitPrice, p[i].UnitLabel = unitPrice(p[i])
		folded[i] = fold(p[i].Name + " " + p[i].Section)
	}
	aMu.Lock()
	aProducts, aSearch, aUpdated = p, folded, time.Now()
	aMu.Unlock()
}

func fetchText(u string) (string, error) {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*")
		req.Header.Set("Accept-Language", "pt-PT,pt;q=0.9,en;q=0.8")
		resp, err := client.Do(req)
		if err != nil {
			last = err
			time.Sleep(time.Duration(400*(1<<attempt)) * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return string(body), nil
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			last = fmt.Errorf("status %d", resp.StatusCode)
			time.Sleep(time.Duration(700*(1<<attempt)) * time.Millisecond)
			continue
		}
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	return "", last
}

type auchanGTMOld struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Price string `json:"price"`
}

type auchanGTM struct {
	ItemID     string `json:"item_id"`
	ItemName   string `json:"item_name"`
	ItemBrand  string `json:"item_brand"`
	Category2  string `json:"item_category2"`
	Category3  string `json:"item_category3"`
	Category4  string `json:"item_category4"`
	Price      string `json:"price"`
	Discount   string `json:"discount"`
	Quantity   string `json:"quantity"`
	ItemVariant string `json:"item_variant"`
}

var (
	gtmNewRe = regexp.MustCompile(`data-gtm-new="([^"]+)"`)
	weightRe = regexp.MustCompile(`(?i)\bkg\b`)
	// Auchan publishes its own unit price per tile; prefer it over parsing names,
	// which misreads "1.200 GR" as 1.2 g and "LATA T30 KG" as 30 kg.
	measuresRe  = regexp.MustCompile(`(?s)<div class="auc-measures">(.*?)</div>\s*</div>`)
	unitPriceRe = regexp.MustCompile(`([0-9]+(?:[.,][0-9]+)?)\s*(?:&euro;|€)\s*/\s*([A-Za-z]+)`)
	gtmRe    = regexp.MustCompile(`data-gtm="([^"]+)"`)
	imgRe    = regexp.MustCompile(`data-srcset="(https://www\.auchan\.pt/dw/image/[^"]+?)"`)
	swRe     = regexp.MustCompile(`[?&]sw=\d+`)
	shRe     = regexp.MustCompile(`[?&]sh=\d+`)
)

// parseAuchanTiles reads one grid page: each tile carries its own JSON blob and
// the first responsive image source that follows it.
func parseAuchanTiles(page string) []Product {
	// Collect the two streams separately and pair them by index. Splitting the
	// page on the tile class does not work: the image container's class is
	// auc-product-tile__image-container__image, so the split cuts each tile in half.
	gtms := gtmNewRe.FindAllStringSubmatch(page, -1)
	olds := gtmRe.FindAllStringSubmatch(page, -1)
	imgs := imgRe.FindAllStringSubmatch(page, -1)
	meas := measuresRe.FindAllStringSubmatch(page, -1)
	out := make([]Product, 0, len(gtms))
	for i, m := range gtms {
		var g auchanGTM
		if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &g); err != nil {
			continue
		}
		if g.ItemName == "" || g.ItemID == "" {
			continue
		}
		// A tile carries two price blobs. data-gtm-new is the line price for the
		// default quantity; data-gtm is the price of one unit, which for loose
		// goods is the price per kg. quantity is the discriminator: 1 means a
		// pack (eggs, 24 un), anything else means sold by weight (0.2 kg of
		// banana at 1.19/kg). Reading "KG" out of the name does not work:
		// "SAL MARINHO 1 KG" is a pack and "MELANCIA KG PRODUTO LOCAL" is loose.
		priceStr := g.Price
		weighed := false
		if q := numOrZero(g.Quantity); q > 0 && math.Abs(q-1) > 0.001 {
			if i < len(olds) {
				var o auchanGTMOld
				if err := json.Unmarshal([]byte(html.UnescapeString(olds[i][1])), &o); err == nil && numOrZero(o.Price) > 0 {
					weighed = true
					priceStr = o.Price
				}
			}
		}
		price, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(priceStr), ",", ".", 1), 64)
		// ParseFloat("NaN") returns NaN with no error, and Go cannot marshal NaN,
		// which silently produced an empty catalogue once. Reject unpriced items.
		if err != nil || math.IsNaN(price) || math.IsInf(price, 0) || price <= 0 {
			continue
		}
		unit := "piece_piece"
		if weighed {
			unit = "kg"
		}
		// take the tile's own unit price when it gives one per kg or per litre
		up, ul := 0.0, ""
		if !weighed && i < len(meas) {
			if m := unitPriceRe.FindStringSubmatch(meas[i][1]); m != nil {
				switch strings.ToLower(m[2]) {
				case "kg":
					up, ul = numOrZero(m[1]), "kg"
				case "l", "lt", "litro", "litros":
					up, ul = numOrZero(m[1]), "L"
				}
			}
		}
		img := ""
		if i < len(imgs) {
			img = html.UnescapeString(imgs[i][1])
			img = swRe.ReplaceAllString(img, "?sw=360")
			img = shRe.ReplaceAllString(img, "&sh=270")
		}
		section := g.Category3
		if section == "" {
			section = g.Category2
		}
		if section == "" {
			section = "Alimentação"
		}
		out = append(out, Product{
			Name:     g.ItemName,
			Price:    price,
			Section:  section,
			ExtID:    g.ItemID,
			ImageURL: img,
			Unit:      unit,
			Weighted:  weighed,
			UnitPrice: up,
			UnitLabel: ul,
		})
	}
	return out
}
func crawlAuchan() ([]Product, error) {
	var out []Product
	seen := map[string]bool{}
	pages := 0
	for _, cgid := range strings.Split(auchanCGIDs, ",") {
		cgid = strings.TrimSpace(cgid)
		if cgid == "" {
			continue
		}
		for start := 0; start < 40000; start += 24 {
			u := fmt.Sprintf("%s?cgid=%s&start=%d&sz=24&format=ajax", auchanGrid, url.QueryEscape(cgid), start)
			page, err := fetchText(u)
			if err != nil || len(page) < 1000 {
				break
			}
			tiles := parseAuchanTiles(page)
			if len(tiles) == 0 {
				break
			}
			for _, p := range tiles {
				if !seen[p.ExtID] {
					seen[p.ExtID] = true
					out = append(out, p)
				}
			}
			pages++
			time.Sleep(140 * time.Millisecond)
		}
		log.Printf("auchan: %s done, %d products so far", cgid, len(out))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("auchan crawl returned nothing")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	return out, nil
}
func loadAuchan() error {
	b, err := os.ReadFile(auchanCatalog)
	if err != nil {
		return err
	}
	var p []Product
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	setAuchan(p)
	return nil
}

// ---- image proxy: fetch once, downscale, cache, serve small ----------------

const imgCacheDir = "imgcache"
const imgMaxSource = 6 << 20 // 6 MB cap on a source image

var imgSem = make(chan struct{}, 8)

func imgHandler(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("u")
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if width <= 0 || width > 1600 {
		width = 360
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "glovo.dhmedia.io" {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	sum := sha1.Sum([]byte(u.String() + "|" + strconv.Itoa(width)))
	name := hex.EncodeToString(sum[:]) + ".jpg"
	path := filepath.Join(imgCacheDir, name)

	if b, err := os.ReadFile(path); err == nil {
		serveImg(w, r, name, b)
		return
	}

	imgSem <- struct{}{}
	defer func() { <-imgSem }()
	if b, err := os.ReadFile(path); err == nil { // another request won the race
		serveImg(w, r, name, b)
		return
	}

	resp, err := client.Get(u.String())
	if err != nil {
		http.Error(w, "fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, "upstream "+resp.Status, http.StatusBadGateway)
		return
	}
	src, err := io.ReadAll(io.LimitReader(resp.Body, imgMaxSource))
	if err != nil || len(src) == 0 {
		http.Error(w, "read failed", http.StatusBadGateway)
		return
	}
	decoded, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		// not decodable: pass the original through rather than a broken tile
		serveImg(w, r, name, src)
		return
	}
	small := boxDownscale(decoded, width)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 72}); err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	out := buf.Bytes()
	_ = os.MkdirAll(imgCacheDir, 0o755)
	_ = os.WriteFile(path, out, 0o644)
	serveImg(w, r, name, out)
}

func serveImg(w http.ResponseWriter, r *http.Request, name string, b []byte) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
	w.Header().Set("ETag", `"`+name+`"`)
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, name) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// boxDownscale averages source blocks, which is fast and clean for photos.
func boxDownscale(src image.Image, width int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if width >= sw || sw == 0 || sh == 0 {
		if width >= sw {
			width = sw
		}
	}
	h := sh * width / sw
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, width, h))
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + y*sh/h
		y1 := b.Min.Y + (y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < width; x++ {
			x0 := b.Min.X + x*sw/width
			x1 := b.Min.X + (x+1)*sw/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					sr += cr >> 8
					sg += cg >> 8
					sb += cb >> 8
					sa += ca >> 8
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(sr / n), uint8(sg / n), uint8(sb / n), uint8(sa / n)})
		}
	}
	return dst
}

// ---- gzip for the text responses ------------------------------------------

type gzipWriter struct {
	http.ResponseWriter
	gz *gzip.Writer
}

func (g *gzipWriter) Write(b []byte) (int, error) { return g.gz.Write(b) }

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzip.NewWriter(w)
		defer gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		next.ServeHTTP(&gzipWriter{w, gz}, r)
	})
}

// ---- handlers -----------------------------------------------------------------

func queryProducts(list []Product, folded []string, q, section, meal, roleWant, sortBy string,
	maxPrice float64, limit, offset int) ([]Product, int) {

	terms := strings.Fields(q)
	out := make([]Product, 0, limit)
	matched, skipped := 0, 0
	for i, p := range list {
		if section != "" && p.Section != section {
			continue
		}
		if roleWant != "" && p.Role != roleWant {
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
			hay := folded[i]
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
	if sortBy == "unit" {
		sort.SliceStable(out, func(i, j int) bool {
			a, b := out[i].UnitPrice, out[j].UnitPrice
			if a <= 0 {
				return false
			}
			if b <= 0 {
				return true
			}
			return a < b
		})
	}
	return out, matched
}

func writeProductResponse(w http.ResponseWriter, r *http.Request, list []Product, folded []string) {
	q := fold(strings.TrimSpace(r.URL.Query().Get("q")))
	section := r.URL.Query().Get("section")
	meal := r.URL.Query().Get("meal")
	roleWant := r.URL.Query().Get("role")
	sortBy := r.URL.Query().Get("sort")
	maxPrice, _ := strconv.ParseFloat(r.URL.Query().Get("max"), 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 48
	}
	out, matched := queryProducts(list, folded, q, section, meal, roleWant, sortBy, maxPrice, limit, offset)
	writeJSON(w, map[string]any{
		"total": len(list), "matched": matched, "offset": offset,
		"count": len(out), "products": out,
	})
}

func handleProducts(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	writeProductResponse(w, r, products, search)
}

func handleAuchanProducts(w http.ResponseWriter, r *http.Request) {
	aMu.RLock()
	defer aMu.RUnlock()
	writeProductResponse(w, r, aProducts, aSearch)
}

func roleCounts(list []Product) map[string]int {
	m := map[string]int{}
	for _, p := range list {
		m[p.Role]++
	}
	return m
}

func handleAuchanSections(w http.ResponseWriter, r *http.Request) {
	aMu.RLock()
	counts := map[string]int{}
	for _, p := range aProducts {
		counts[p.Section]++
	}
	aMu.RUnlock()
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

func handleAuchanStats(w http.ResponseWriter, r *http.Request) {
	aMu.RLock()
	n, at, list := len(aProducts), aUpdated, aProducts
	aMu.RUnlock()
	writeJSON(w, map[string]any{
		"products": n, "updated": at, "roles": roleCounts(list),
		"source": "auchan", "refresh": map[string]any{"running": false},
	})
}

// stapleOffers finds, for each pantry staple, the cheapest product in a list.
func stapleOffers(list []Product) []map[string]any {
	res := make([]map[string]any, 0, len(stapleDefs))
	for _, d := range stapleDefs {
		var best *Product
		for i := range list {
			p := &list[i]
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
			if best == nil || p.Price < best.Price {
				best = p
			}
		}
		var prod map[string]any
		if best != nil {
			prod = map[string]any{"name": best.Name, "price": best.Price, "imageUrl": best.ImageURL, "section": best.Section}
		}
		res = append(res, map[string]any{
			"key": d.Key, "label": d.Label, "impliedBy": d.ImpliedBy,
			"triggers": d.Triggers, "product": prod,
		})
	}
	return res
}

func handleAuchanStaples(w http.ResponseWriter, r *http.Request) {
	aMu.RLock()
	list := aProducts
	aMu.RUnlock()
	writeJSON(w, map[string]any{"staples": stapleOffers(list)})
}

func handleAuchanRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	go func() {
		p, err := crawlAuchan()
		if err != nil {
			log.Printf("auchan refresh failed: %v", err)
			return
		}
		b, err := json.Marshal(p)
		if err != nil {
			log.Printf("auchan marshal failed: %v", err)
			return
		}
		if err := os.WriteFile(auchanCatalog, b, 0o644); err != nil {
			log.Printf("auchan write failed: %v", err)
			return
		}
		setAuchan(p)
		log.Printf("auchan refreshed: %d products", len(p))
	}()
	writeJSON(w, map[string]any{"started": true})
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
	writeJSON(w, map[string]any{"products": n, "updated": at, "refresh": c, "roles": roleCounts(products), "source": "glovo"})
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
	crawlA := flag.Bool("crawl-auchan", false, "crawl Auchan's food catalogue, save it, and exit")
	flag.Parse()

	if *crawlA {
		log.Print("crawling Auchan alimentacao...")
		p, err := crawlAuchan()
		if err != nil {
			log.Fatalf("auchan crawl failed: %v", err)
		}
		b, _ := json.Marshal(p)
		if err := os.WriteFile(auchanCatalog, b, 0o644); err != nil {
			log.Fatalf("write failed: %v", err)
		}
		secs := map[string]int{}
		for _, x := range p {
			secs[x.Section]++
		}
		log.Printf("wrote %d products to %s across %d sections", len(p), auchanCatalog, len(secs))
		return
	}

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
	log.Printf("loaded %d Glovo products", n)
	if err := loadAuchan(); err != nil {
		log.Printf("no Auchan catalogue yet (%v)", err)
	} else {
		aMu.RLock()
		log.Printf("loaded %d Auchan products", len(aProducts))
		aMu.RUnlock()
	}

	if *doRefresh {
		go refresh()
	}

	// both catalogues serve the same app; the path picks the data source
	appPage := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := pageFS.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}))
	http.Handle("/", appPage)
	http.Handle("/auchan-direct", appPage)
	http.Handle("/auchan-direct/", appPage)
	http.HandleFunc("/hero.jpg", func(w http.ResponseWriter, r *http.Request) {
		b, err := pageFS.ReadFile("hero.jpg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		_, _ = w.Write(b)
	})
	http.HandleFunc("/img", imgHandler)
	http.Handle("/api/products", withGzip(http.HandlerFunc(handleProducts)))
	http.Handle("/api/sections", withGzip(http.HandlerFunc(handleSections)))
	http.Handle("/api/staples", withGzip(http.HandlerFunc(handleStaples)))
	http.Handle("/api/stats", withGzip(http.HandlerFunc(handleStats)))
	http.HandleFunc("/api/refresh", handleRefresh)
	http.Handle("/api/auchan/products", withGzip(http.HandlerFunc(handleAuchanProducts)))
	http.Handle("/api/auchan/sections", withGzip(http.HandlerFunc(handleAuchanSections)))
	http.Handle("/api/auchan/staples", withGzip(http.HandlerFunc(handleAuchanStaples)))
	http.Handle("/api/auchan/stats", withGzip(http.HandlerFunc(handleAuchanStats)))
	http.HandleFunc("/api/auchan/refresh", handleAuchanRefresh)

	addr := *host + ":" + strconv.Itoa(*port)
	log.Printf("meal builder on http://localhost:%d  (%s)", *port, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// keep net/url imported for future query building without churn
var _ = url.Values{}















