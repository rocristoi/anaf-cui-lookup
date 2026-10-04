package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const anafURL = "https://webservicesp.anaf.ro/api/PlatitorTvaRest/v9/tva"

// maxBatch is the maximum number of CUIs ANAF accepts per request.
const maxBatch = 100

type Company struct {
	Nume              string  `json:"nume"`
	CUI               string  `json:"cui"`
	CUIFormatted      string  `json:"cuiFormatted"`
	RegCom            *string `json:"reg_com"`
	Strada            *string `json:"strada"`
	Judet             *string `json:"judet"`
	Locatie           *string `json:"locatie"`
	CodPostal         *string `json:"cod_postal"`
	AdresaCompleta    *string `json:"adresaCompleta"`
	Telefon           *string `json:"telefon"`
	Email             *string `json:"email"`
	ScpTVA            bool    `json:"scpTVA"`
	StatusInactivi    bool    `json:"statusInactivi"`
	StareInregistrare *string `json:"stare_inregistrare"`
}

type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

type anafSediu struct {
	Strada    string `json:"sdenumire_Strada"`
	Numar     string `json:"snumar_Strada"`
	Judet     string `json:"sdenumire_Judet"`
	Localitate string `json:"sdenumire_Localitate"`
	CodPostal string `json:"scod_Postal"`
	Detalii   string `json:"sdetalii_Adresa"`
}

type anafFiscal struct {
	Strada     string `json:"ddenumire_Strada"`
	Numar      string `json:"dnumar_Strada"`
	Judet      string `json:"ddenumire_Judet"`
	Localitate string `json:"ddenumire_Localitate"`
	CodPostal  string `json:"dcod_Postal"`
}

type anafItem struct {
	General struct {
		CUI       json.Number `json:"cui"`
		Denumire  string      `json:"denumire"`
		Adresa    string      `json:"adresa"`
		Telefon   string      `json:"telefon"`
		CodPostal string      `json:"codPostal"`
		NrRegCom  string      `json:"nrRegCom"`
		Stare     string      `json:"stare_inregistrare"`
	} `json:"date_generale"`
	Tva struct {
		ScpTVA bool `json:"scpTVA"`
	} `json:"inregistrare_scop_Tva"`
	Inactiv struct {
		StatusInactivi bool `json:"statusInactivi"`
	} `json:"stare_inactiv"`
	Sediu  anafSediu  `json:"adresa_sediu_social"`
	Fiscal anafFiscal `json:"adresa_domiciliu_fiscal"`
}

type anafResponse struct {
	Found    []anafItem        `json:"found"`
	NotFound []json.RawMessage `json:"notFound"`
}

var nonDigit = regexp.MustCompile(`\D`)
var spaces = regexp.MustCompile(`\s+`)

// normalizeCui strips the RO prefix, spaces and punctuation. Returns "" if invalid.
func normalizeCui(in string) string {
	s := spaces.ReplaceAllString(in, "")
	if len(s) >= 2 && strings.EqualFold(s[:2], "RO") {
		s = s[2:]
	}
	s = nonDigit.ReplaceAllString(s, "")
	if len(s) < 2 || len(s) > 10 || strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}

func anafToday() string {
	loc, err := time.LoadLocation("Europe/Bucharest")
	if err != nil {
		loc = time.UTC
	}
	return time.Now().In(loc).Format("2006-01-02")
}

func clean(s string) *string {
	t := strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if t == "" {
		return nil
	}
	return &t
}

func first(vals ...*string) *string {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func street(parts ...string) *string {
	var out []string
	for _, p := range parts {
		if c := clean(p); c != nil {
			out = append(out, *c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	s := strings.Join(out, " ")
	return &s
}

func mapItem(it anafItem) Company {
	g, s, f := it.General, it.Sediu, it.Fiscal
	cui := nonDigit.ReplaceAllString(g.CUI.String(), "")
	cf := cui
	if it.Tva.ScpTVA {
		cf = "RO" + cui
	}
	strada := street(s.Strada, s.Numar)
	if d := clean(s.Detalii); d != nil {
		if strada != nil {
			joined := *strada + ", " + *d
			strada = &joined
		} else {
			strada = d
		}
	}
	strada = first(strada, street(f.Strada, f.Numar))

	nume := ""
	if n := clean(g.Denumire); n != nil {
		nume = *n
	}
	return Company{
		Nume:              nume,
		CUI:               cui,
		CUIFormatted:      cf,
		RegCom:            clean(g.NrRegCom),
		Strada:            strada,
		Judet:             first(clean(s.Judet), clean(f.Judet)),
		Locatie:           first(clean(s.Localitate), clean(f.Localitate)),
		CodPostal:         first(clean(s.CodPostal), clean(f.CodPostal), clean(g.CodPostal)),
		AdresaCompleta:    clean(g.Adresa),
		Telefon:           clean(g.Telefon),
		ScpTVA:            it.Tva.ScpTVA,
		StatusInactivi:    it.Inactiv.StatusInactivi,
		StareInregistrare: clean(g.Stare),
	}
}

type anafClient struct {
	http *http.Client
}

func newAnafClient(timeout time.Duration) *anafClient {
	return &anafClient{http: &http.Client{Timeout: timeout}}
}

// lookup queries ANAF for the given normalized CUIs. It returns the companies
// found, keyed by CUI. CUIs missing from the map were not found.
func (c *anafClient) lookup(ctx context.Context, cuis []string) (map[string]Company, *apiError) {
	date := anafToday()
	payload := make([]map[string]any, 0, len(cuis))
	for _, cui := range cuis {
		n, _ := strconv.ParseInt(cui, 10, 64)
		payload = append(payload, map[string]any{"cui": n, "data": date})
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anafURL, bytes.NewReader(body))
	if err != nil {
		return nil, &apiError{502, "unavailable", "Could not build the ANAF request."}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil || strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "Timeout") {
			return nil, &apiError{504, "timeout", "The ANAF request timed out."}
		}
		return nil, &apiError{502, "unavailable", "Could not reach the ANAF service."}
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusTooManyRequests {
		return nil, &apiError{503, "rate_limited", "ANAF is busy, retry in a few seconds."}
	}

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, &apiError{502, "unavailable", "Could not read the ANAF response."}
	}
	var parsed anafResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		if res.StatusCode >= 400 {
			return nil, &apiError{502, "unavailable", fmt.Sprintf("ANAF responded with an error (%d).", res.StatusCode)}
		}
		return nil, &apiError{502, "parse_error", "Invalid response from ANAF."}
	}
	// ANAF can answer 404 with a valid { found, notFound } body.
	if res.StatusCode >= 500 || (res.StatusCode >= 400 && res.StatusCode != http.StatusNotFound) {
		return nil, &apiError{502, "unavailable", fmt.Sprintf("ANAF is unavailable (%d).", res.StatusCode)}
	}

	out := make(map[string]Company, len(parsed.Found))
	for _, it := range parsed.Found {
		co := mapItem(it)
		if co.Nume == "" || co.CUI == "" {
			continue
		}
		out[co.CUI] = co
	}
	return out, nil
}
