package main

import (
	"bufio"
	"errors"
	"gopkg.in/tomb.v2"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/DataDog/datadog-go/statsd"
	log "github.com/cihub/seelog"
)

type Client struct {
	client *statsd.Client
	ip     net.IP
	port   int32
	sleep  int32
	flows  *FlowMap
	tags   []string
	lookup map[string]string
	t      tomb.Tomb
}

const (
	statsdBufflen = 5
	statsdSleep   = 30
)

func memorySize() (uint64, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, err
	}

	s := bufio.NewScanner(f)
	if !s.Scan() {
		return 0, errors.New("/proc/meminfo parse error")
	}

	l := s.Text()
	fs := strings.Fields(l)
	if len(fs) != 3 || fs[2] != "kB" {
		return 0, errors.New("/proc/meminfo parse error")
	}

	kb, err := strconv.ParseUint(fs[1], 10, 64)
	if err != nil {
		return 0, err
	}

	//return bytes
	return kb * 1024, nil
}

func NewClient(ip net.IP, port int32, sleep int32, flows *FlowMap, lookup map[string]string, tags []string) (*Client, error) {
	cli, err := statsd.NewBuffered(net.JoinHostPort(ip.String(), strconv.Itoa(int(port))), statsdBufflen)
	if err != nil {
		cli = nil
		log.Errorf("Error instantiating stats Statter: %v", err)
		return nil, err
	}

	r := &Client{
		client: cli,
		port:   port,
		sleep:  sleep,
		flows:  flows,
		tags:   tags,
		lookup: lookup,
	}
	r.t.Go(r.Report)
	return r, nil
}

func (r *Client) Stop() error {
	r.t.Kill(nil)
	return r.t.Wait()
}

func (r *Client) submit(key, metric string, value float64, tags []string, asHistogram bool) error {
	var err error
	if asHistogram {
		err = r.client.Histogram(metric, value, tags, 1)
	} else {
		err = r.client.Gauge(metric, value, tags, 1)
	}
	if err != nil {
		log.Infof("There was an issue reporting metric: [%s] %s = %v - error: %v", key, metric, value, err)
		return err
	} else {
		log.Infof("Reported successfully! Metric: [%s] %s = %v - tags: %v", key, metric, value, tags)
	}
	return nil
}

func (r *Client) Report() error {
	defer r.client.Close()

	log.Infof("Started reporting.")

	memsize, err := memorySize()
	if err != nil {
		log.Warnf("Error getting memory size. Relying on OOM to keep process in check. Err: %v", err)
	}

	ticker := time.NewTicker(time.Duration(r.sleep) * time.Second)
	done := false
	var memstats runtime.MemStats
	var pct float64
	for !done {
		select {
		case key := <-r.flows.Expire:
			r.flows.Delete(key)
			log.Infof("Flow expired: [%s]", key)
		case <-ticker.C:
			flush := false
			now := time.Now().Unix()

			runtime.ReadMemStats(&memstats)
			if memsize > 0 {
				pct = float64(memstats.Alloc) / float64(memsize)
			} else {
				pct = 0
			}

			if pct >= FORCE_FLUSH_PCT { //memory out of control
				flush = true
				log.Warnf("Forcing flush - memory consumption above maximum allowed system usage: %v %%", pct*100)
			}

			r.flows.Lock()
			for k := range r.flows.Map {
				flow, e := r.flows.GetUnsafe(k)
				flow.Lock()
				if e && flow.Sampled > 0 {
					success := true
					value := float64(flow.SRTT) * float64(time.Nanosecond) / float64(time.Millisecond)
					value_jitter := float64(flow.Jitter) * float64(time.Nanosecond) / float64(time.Millisecond)
					value_last := float64(flow.Last) * float64(time.Nanosecond) / float64(time.Millisecond)

					srcHost, ok := r.lookup[flow.Src.String()]
					if !ok {
						srcHost = flow.Src.String()
					}
					dstHost, ok := r.lookup[flow.Dst.String()]
					if !ok {
						dstHost = flow.Dst.String()
					}

					tags := []string{"src:" + srcHost, "dst:" + dstHost}
					tags = append(tags, r.tags...)

					metric := "system.net.tcp.rtt.avg"
					err := r.submit(k, metric, value, tags, false)
					if err != nil {
						success = false
					}
					metric = "system.net.tcp.rtt.jitter"
					err = r.submit(k, metric, value_jitter, tags, false)
					if err != nil {
						success = false
					}
					metric = "system.net.tcp.rtt"
					err = r.submit(k, metric, value_last, tags, false)
					if err != nil {
						success = false
					}
					if success {
						log.Debugf("Reported successfully on: %v", k)
					}
				}
				if flush || (now-flow.LastFlush) > FLUSH_IVAL {
					log.Debugf("Flushing book-keeping for long-lived flow: %v", k)
					flow.Flush()
				}
				flow.Unlock()
			}
			r.flows.Unlock()
		case <-r.t.Dying():
			log.Infof("Done reporting.")
			done = true
		}
	}

	return nil
}
