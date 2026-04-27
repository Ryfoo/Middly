// Command weather is a tiny demo client for middly. It fetches a current
// forecast from wttr.in (free, no API key) several times in a row so you
// can watch the first request go to the network and the rest get served
// from middly's cache in well under a millisecond.
//
//	# terminal 1
//	./middly --routes='/wttr=https://wttr.in'
//
//	# terminal 2
//	go run ./examples/weather --city=Berlin --runs=5
//
// To bypass middly and hit wttr.in directly (for comparison):
//
//	go run ./examples/weather --base=https://wttr.in --city=Berlin --runs=5
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Subset of the wttr.in `?format=j1` payload that we care about.
type wttrResp struct {
	CurrentCondition []struct {
		TempC           string `json:"temp_C"`
		TempF           string `json:"temp_F"`
		Humidity        string `json:"humidity"`
		WindSpeedKmph   string `json:"windspeedKmph"`
		ObservationTime string `json:"observation_time"`
		WeatherDesc     []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
	} `json:"current_condition"`
	NearestArea []struct {
		AreaName []struct {
			Value string `json:"value"`
		} `json:"areaName"`
		Country []struct {
			Value string `json:"value"`
		} `json:"country"`
	} `json:"nearest_area"`
}

func main() {
	base := flag.String("base", "http://localhost:8080/wttr",
		"weather API base URL — point at middly, or use https://wttr.in to bypass it")
	city := flag.String("city", "Berlin", "city name (anything wttr.in understands)")
	runs := flag.Int("runs", 3, "how many times to fetch — shows the cache-hit speedup")
	flag.Parse()

	target := fmt.Sprintf("%s/%s?format=j1",
		strings.TrimRight(*base, "/"),
		url.PathEscape(*city),
	)
	fmt.Printf("GET %s\n\n", target)
	fmt.Printf("%-4s  %-9s  %-22s  %-7s  %-9s  %-8s  %-5s  %s\n",
		"run", "obs", "where", "tempC", "wind", "humidity", "cache", "latency")
	fmt.Println(strings.Repeat("-", 92))

	for i := 1; i <= *runs; i++ {
		start := time.Now()
		w, hdr, err := fetch(target)
		dur := time.Since(start)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[run %d] %v\n", i, err)
			os.Exit(1)
		}
		cache := hdr.Get("X-Cache")
		if cache == "" {
			cache = "MISS"
		}

		var (
			obs, where, temp, wind, hum string
		)
		if len(w.CurrentCondition) > 0 {
			c := w.CurrentCondition[0]
			obs = c.ObservationTime
			temp = c.TempC + "°C"
			wind = c.WindSpeedKmph + " km/h"
			hum = c.Humidity + "%"
		}
		if len(w.NearestArea) > 0 && len(w.NearestArea[0].AreaName) > 0 {
			a := w.NearestArea[0]
			where = a.AreaName[0].Value
			if len(a.Country) > 0 {
				where += ", " + a.Country[0].Value
			}
		}

		fmt.Printf("%-4d  %-9s  %-22.22s  %-7s  %-9s  %-8s  %-5s  %s\n",
			i, obs, where, temp, wind, hum, cache, dur)
	}
}

func fetch(target string) (wttrResp, http.Header, error) {
	var w wttrResp
	resp, err := http.Get(target)
	if err != nil {
		return w, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return w, resp.Header, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	err = json.NewDecoder(resp.Body).Decode(&w)
	return w, resp.Header, err
}
