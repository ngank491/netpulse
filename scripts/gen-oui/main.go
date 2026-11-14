//go:build ignore

// gen-oui converts the IEEE public OUI registry (https://standards-oui.ieee.org/oui/oui.csv)
// into internal/app/oui.txt.gz: "AABBCC<TAB>Short vendor name" lines, sorted.
//
//	curl -A Mozilla -o oui.csv https://standards-oui.ieee.org/oui/oui.csv
//	go run scripts/gen-oui/main.go oui.csv internal/app/oui.txt.gz
package main

import (
	"compress/gzip"
	"encoding/csv"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

var legal = map[string]bool{"co": true, "co.": true, "ltd": true, "ltd.": true, "inc": true, "inc.": true, "corp": true, "corp.": true,
	"corporation": true, "llc": true, "gmbh": true, "limited": true, "ag": true, "sa": true, "s.a.": true, "b.v.": true, "bv": true,
	"pte": true, "pty": true, "srl": true, "oy": true, "ab": true, "as": true, "company": true, "plc": true, "kg": true, "s.p.a.": true,
	"co.,ltd": true, "co.,ltd.": true, "co.ltd": true, "co.,": true, "the": true, "&": true}

var hexRe = regexp.MustCompile(`^[0-9A-F]{6}$`)

func casing(w string) string {
	if strings.ToUpper(w) != w {
		return w // already mixed case, trust it
	}
	parts := strings.Split(w, "-")
	for i, p := range parts {
		if len(p) > 3 {
			parts[i] = p[:1] + strings.ToLower(p[1:])
		}
	}
	return strings.Join(parts, "-")
}

func short(org string) string {
	org = strings.TrimSpace(org)
	for _, sep := range []string{",", " (", "/"} {
		if i := strings.Index(org, sep); i > 0 {
			org = org[:i]
		}
	}
	var words []string
	for _, w := range strings.Fields(org) {
		if legal[strings.ToLower(w)] {
			continue
		}
		words = append(words, casing(strings.Trim(w, ".")))
		if len(words) == 2 {
			break
		}
	}
	s := strings.Join(words, " ")
	if len(s) > 28 {
		s = s[:28]
	}
	return strings.TrimSpace(s)
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gen-oui oui.csv out.txt.gz")
		os.Exit(2)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		panic(err)
	}
	seen := map[string]string{}
	for _, r := range rows[1:] {
		if len(r) < 3 || r[0] != "MA-L" || !hexRe.MatchString(r[1]) {
			continue
		}
		if n := short(r[2]); n != "" {
			seen[r[1]] = n
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out, err := os.Create(os.Args[2])
	if err != nil {
		panic(err)
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestCompression)
	for _, k := range keys {
		fmt.Fprintf(zw, "%s\t%s\n", k, seen[k])
	}
	zw.Close()
	out.Close()
	fmt.Println("wrote", len(keys), "entries")
}
