package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
	log "github.com/sirupsen/logrus"
)

var cache RTBusCache
var cacheLock sync.RWMutex
var cacheRefreshInterval = time.Minute
var backendSvcURL string
var defaultReqTimeout = 500 * time.Millisecond

func main() {
	// init cache
	cache = make(map[string][]RTBus)
	// init backendSvcURL
	backendSvcURL = "http://127.0.0.1:5000"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cacheAllBuses(ctx)

	// start API server
	r := gin.Default()
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"message": "pong",
		})
	})

	r.GET("/closeststops", func(c *gin.Context) {
		lat := c.Query("lat")
		if len(lat) == 0 {
			c.JSON(400, gin.H{"error": "no latitude provided"})
			return
		}
		parsedLat, err := strconv.ParseFloat(lat, 64)
		if err != nil {
			log.Errorf("failed to parse latitude: %s", err.Error())
			c.JSON(400, gin.H{"error": fmt.Sprintf("bad latitude provided, got %v", lat)})
			return
		}
		lon := c.Query("lon")
		if len(lon) == 0 {
			c.JSON(400, gin.H{"error": "no longitude provided"})
			return
		}
		parsedLon, err := strconv.ParseFloat(lon, 64)
		if err != nil {
			log.Errorf("failed to parse longitude: %s", err.Error())
			c.JSON(400, gin.H{"error": fmt.Sprintf("bad longitude provided, got %v", lon)})
			return
		}
		ctx, cancel := context.WithTimeout(c, defaultReqTimeout)
		defer cancel()
		stops, err := getClosestStops(ctx, parsedLat, parsedLon)
		if err != nil {
			log.Errorf("getClosestStops request failed: %s", err.Error())
			c.JSON(500, gin.H{"error": "failed to get closest stops"})
			return
		}

		c.JSON(200, gin.H{"stops": stops})
	})

	r.GET("/closestbuses", func(c *gin.Context) {
		// prefer stopID query param; take lat/lon
		// as fallback
		var stop Stop
		var lat, lon float64
		stopID := c.Query("stopid")
		if len(stopID) > 0 {
			ctx, cancel := context.WithTimeout(c, defaultReqTimeout)
			defer cancel()
			var err error
			stop, err = getStopInfo(ctx, stopID)
			if err != nil {
				log.Errorf("failed to get stop info: %s", err.Error())
				c.JSON(500, gin.H{"error": fmt.Sprintf("unable to get stop info for %+v", stopID)})
				return
			}
			lat, _ = strconv.ParseFloat(stop.Lat, 64)
			lon, _ = strconv.ParseFloat(stop.Lon, 64)
		} else {
			queryLat := c.Query("lat")
			queryLon := c.Query("lon")
			if len(queryLat) == 0 || len(queryLon) == 0 {
				c.JSON(400, gin.H{"error": "missing location data (stop ID or lat/lon) is missing"})
				return
			}
			var err error
			lat, err = strconv.ParseFloat(queryLat, 64)
			if err != nil {
				log.Errorf("failed to parse latitude: %s", err.Error())
				c.JSON(400, gin.H{"error": fmt.Sprintf("bad latitude provided, got %v", lat)})
				return
			}
			lon, err = strconv.ParseFloat(queryLon, 64)
			if err != nil {
				log.Errorf("failed to parse longitude: %s", err.Error())
				c.JSON(400, gin.H{"error": fmt.Sprintf("bad longitude provided, got %v", lon)})
				return
			}

			// call getClosestStops
			stopsCtx, stopsCtxCancel := context.WithTimeout(c, defaultReqTimeout)
			defer stopsCtxCancel()

			closestStops, err := getClosestStops(stopsCtx, lat, lon)
			if err != nil {
				log.Errorf("failed to get closest stops: %s", err.Error())
				c.JSON(500, gin.H{"error": "failed to get closest stops"})
				return
			}

			// we successfully got closest stops, pick one for now and find routes that serve it
			stop = closestStops[0]
			fmt.Println("lat/lon qparam stop:", stop)
		}
		fmt.Println(cache["186"])
		routesCtx, routesCtxCancel := context.WithTimeout(c, defaultReqTimeout)
		defer routesCtxCancel()
		routesForStop, err := getRoutesForStop(routesCtx, stop.ID)
		if err != nil {
			log.Errorf("failed to get routes for stop %d: %s", stop.ID, err.Error())
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to get routes for stop %d", stop.ID)})
			return
		}
		fmt.Println("routesForStop:", routesForStop)
		// then, find buses in the cache for those routes
		realtimeBusesForRoute := []RTBus{}
		for _, route := range routesForStop {
			fmt.Println("route:", route)
			cacheLock.RLock()
			fmt.Println(fmt.Sprintf("realtime buses serving stop %d: %+v", stop.ID, cache[route.ShortName]))
			if rtBuses, ok := cache[route.ShortName]; ok {
				realtimeBusesForRoute = append(realtimeBusesForRoute, rtBuses...)
			}
			cacheLock.RUnlock()
		}
		fmt.Println("num of realtime buses:", len(realtimeBusesForRoute))
		radiusInMiles := .5
		// finally, get buses within the radius
		busesInRadius, err := getBusesWithinRadius(realtimeBusesForRoute, radiusInMiles, lat, lon)
		if err != nil {
			log.Errorf("failed to get buses within radius: %s", err.Error())
			c.JSON(500, gin.H{"error": "failed to get buses within radius"})
			return
		}
		fmt.Println("number of buses in radius:", len(busesInRadius))
		fmt.Println(fmt.Sprintf("buses in %f mi radius: %+v", radiusInMiles, busesInRadius))
		c.JSON(200, gin.H{"closestBuses": busesInRadius})
	})

	r.Run()
}

type Route struct {
	ShortName string `json:"shortName"`
	LongName  string `json:"longName"`
}

type RoutesForStopResp struct {
	Routes []Route `json:"routes"`
	Error  string  `json:"error"`
}

func getRoutesForStop(ctx context.Context, stopID int) ([]Route, error) {
	var decodedResp RoutesForStopResp
	fullURL := fmt.Sprintf(backendSvcURL+"/routesforstop?id=%d", stopID)
	ctx, cancel := context.WithTimeout(ctx, defaultReqTimeout)
	defer cancel()
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		log.Errorf("error creating request object: %s", err.Error())
		return []Route{}, err
	}
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []Route{}, err
	}
	err = json.NewDecoder(resp.Body).Decode(&decodedResp)
	if err != nil {
		return []Route{}, err
	}
	if len(decodedResp.Error) > 0 {
		return []Route{}, errors.New(decodedResp.Error)
	}
	fmt.Println(decodedResp)
	return decodedResp.Routes, err
}

func getBusesWithinRadius(buses []RTBus, radius, lat, lon float64) ([]RTBus, error) {
	defaultGeoLevel := 12 // 3.31km^2 - 6.38km^2
	earthRadiusKm := 6371.01
	latLong := s2.LatLngFromDegrees(lat, lon)
	point := s2.PointFromLatLng(latLong)
	kmRadius := 1.61 * radius
	angle := s1.Angle(kmRadius / earthRadiusKm)
	sphereCap := s2.CapFromCenterAngle(point, angle)
	region := s2.Region(sphereCap)
	regionCoverer := &s2.RegionCoverer{
		MaxLevel: defaultGeoLevel,
		MinLevel: defaultGeoLevel,
	}
	busesInRadius := []RTBus{}
	cellUnion := regionCoverer.Covering(region)
	for _, cellID := range cellUnion {
		c := s2.CellFromCellID(cellID)
		loop := s2.LoopFromCell(c)
		for _, bus := range buses {
			// TODO: change Latitude field to float
			// so that this isn't necessary here
			parsedLat, _ := strconv.ParseFloat(bus.Latitude, 64)
			parsedLon, _ := strconv.ParseFloat(bus.Longitude, 64)
			busLL := s2.LatLngFromDegrees(parsedLat, parsedLon)
			busPt := s2.PointFromLatLng(busLL)
			if loop.ContainsPoint(busPt) {
				busesInRadius = append(busesInRadius, bus)
			}
		}
	}
	return busesInRadius, nil
}

type Stop struct {
	Lat    string  `json:"lat"`
	Lon    string  `json:"lon"`
	ID     int     `json:"id"`
	Name   string  `json:"name"`
	Routes []Route `json:"routes"` // routes that serve this stop
}
type StopsResp struct {
	Error string `json:"error"`
	Stops []Stop `json:"stops"`
}

type StopResp struct {
	Error string
	Stop  Stop
}

func getStopInfo(ctx context.Context, id string) (Stop, error) {
	var decodedResp StopResp
	fullURL := backendSvcURL + "/stop?id=" + id
	ctx, cancel := context.WithTimeout(ctx, defaultReqTimeout)
	defer cancel()
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		log.Errorf("error creating request object: %s", err.Error())
		return Stop{}, err
	}
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Stop{}, err
	}
	err = json.NewDecoder(resp.Body).Decode(&decodedResp.Stop)
	if err != nil {
		return Stop{}, err
	}
	if len(decodedResp.Error) > 0 {
		return Stop{}, errors.New(decodedResp.Error)
	}
	return decodedResp.Stop, err
}

func getClosestStops(ctx context.Context, lat, lon float64) ([]Stop, error) {
	var decodedResp StopsResp
	// make req to BE svc
	fullURL := backendSvcURL + "/closeststops?lat=" + strconv.FormatFloat(lat, 'g', -1, 64) + "&lon=" + strconv.FormatFloat(lon, 'g', -1, 64)
	ctx, cancel := context.WithTimeout(ctx, defaultReqTimeout)
	defer cancel()
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		log.Errorf("error creating request object: %s", err.Error())
		return []Stop{}, err
	}
	req = req.WithContext(ctx)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return []Stop{}, err
	}
	err = json.NewDecoder(resp.Body).Decode(&decodedResp)
	if err != nil {
		return []Stop{}, err
	}
	if len(decodedResp.Error) > 0 {
		return []Stop{}, errors.New(decodedResp.Error)
	}
	return decodedResp.Stops, err
}

type RTBus struct {
	Adherence string `json:"ADHERENCE"`
	Direction string `json:"DIRECTION"`
	Latitude  string `json:"LATITUDE"`
	Longitude string `json:"LONGITUDE"`
	LastSeen  string `json:"MSGTIME"`
	Route     string `json:"ROUTE"`
	StopID    string `json:"STOPID"`
	Timepoint string `json:"TIMEPOINT"`
	TripID    string `json:"TRIPID"`
	Vehicle   string `json:"VEHICLE"`
}

type RTBusCache map[string][]RTBus

func cacheAllBuses(ctx context.Context) {
	initialExponentialDelay := 500 * time.Millisecond
	var numOfFailedAttempts int
	delay := cacheRefreshInterval
	for {
		select {
		case <-ctx.Done():
			return
		default:
			req, err := http.NewRequest("GET", "http://developer.itsmarta.com/BRDRestService/RestBusRealTimeService/GetAllBus", nil)
			if err != nil {
				log.Errorf("failed to create context to get real-time bus data: %s", err.Error())
				return
			}
			req = req.WithContext(ctx)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				numOfFailedAttempts++
				log.Errorf("unable to get real-time bus data: %s", err.Error())
				// exponential backoff
				time.Sleep(initialExponentialDelay)
				initialExponentialDelay *= 2
				continue
			} else if numOfFailedAttempts > 0 {
				// reset exponential delay and failed attempts
				numOfFailedAttempts = 0
				initialExponentialDelay = 500 * time.Millisecond
			}
			defer resp.Body.Close()

			var realtimeBuses []RTBus
			err = json.NewDecoder(resp.Body).Decode(&realtimeBuses)
			if err != nil {
				log.Errorf("failed to decode response body: %s", err.Error())
				continue
			}

			// unmarshaling succeeded; clear the cache
			cacheLock.Lock()
			cache = make(map[string][]RTBus)
			// refresh the cache
			for _, bus := range realtimeBuses {
				var cachedRoute []RTBus
				if cachedRoute = cache[bus.Route]; cachedRoute != nil {
					cachedRoute = append(cachedRoute, bus)
				} else {
					cachedRoute = []RTBus{bus}
				}
				cache[bus.Route] = cachedRoute
			}
			cacheLock.Unlock()
			time.Sleep(delay)
		}
	}
}
