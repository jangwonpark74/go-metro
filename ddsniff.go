package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gopkg.in/tomb.v2"

	log "github.com/Sirupsen/logrus"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
)

type DatadogDecoder struct {
	eth           layers.Ethernet
	dot1q         layers.Dot1Q
	ip4           layers.IPv4
	ip6           layers.IPv6
	ip6extensions layers.IPv6ExtensionSkipper
	tcp           layers.TCP
	payload       gopacket.Payload
	parser        *gopacket.DecodingLayerParser
	decoded       []gopacket.LayerType
}

func NewDatadogDecoder() *DatadogDecoder {
	d := &DatadogDecoder{
		decoded: make([]gopacket.LayerType, 0, 4),
	}
	d.parser = gopacket.NewDecodingLayerParser(layers.LayerTypeEthernet,
		&d.eth, &d.dot1q, &d.ip4, &d.ip6,
		&d.ip6extensions, &d.tcp, &d.payload)

	return d
}

// We use a DecodingLayerParser here instead of a simpler PacketSource.
// This approach should be measurably faster, but is also more rigid.
// PacketSource will handle any known type of packet safely and easily,
// but DecodingLayerParser will only handle those packet types we
// specifically pass in.  This trade-off can be quite useful, though, in
// high-throughput situations.
type DatadogSniffer struct {
	Iface      string
	Snaplen    int
	Filter     string
	ExpTTL     int
	IdleTTL    int
	Soften     bool
	statsdIP   string
	statsdPort int32
	pcapHandle *pcap.Handle
	decoder    *DatadogDecoder
	hostIPs    map[string]bool
	flows      *FlowMap
	reporter   *Client
	config     Config
	t          tomb.Tomb
}

func NewDatadogSniffer(instcfg InitConfig, cfg Config, filter string) (*DatadogSniffer, error) {
	//log.Printf("new stream %v:%v started", net, transport)
	d := &DatadogSniffer{
		Iface:      cfg.Interface,
		Snaplen:    instcfg.Snaplen,
		Filter:     filter,
		ExpTTL:     instcfg.ExpTTL,
		IdleTTL:    instcfg.IdleTTL,
		Soften:     false,
		statsdIP:   instcfg.StatsdIP,
		statsdPort: int32(instcfg.StatsdPort),
		pcapHandle: nil,
		hostIPs:    make(map[string]bool),
		flows:      NewFlowMap(),
		config:     cfg,
	}
	d.decoder = NewDatadogDecoder()
	var err error
	d.reporter, err = NewClient(net.ParseIP(d.statsdIP), d.statsdPort, statsdSleep, d.flows, d.config.Tags)
	if err != nil {
		return nil, err
	}

	return d, nil
}

func readUint32(data []byte) (ret uint32) {
	buf := bytes.NewBuffer(data)
	binary.Read(buf, binary.BigEndian, &ret)
	return
}

func GetTimestamps(tcp *layers.TCP) (uint32, uint32, error) {
	for i := range tcp.Options {
		if tcp.Options[i].OptionType == 8 {
			ts := readUint32(tcp.Options[i].OptionData[:4])
			tsecr := readUint32(tcp.Options[i].OptionData[4:])
			return ts, tsecr, nil
		}
	}
	return 0, 0, errors.New("No TCP timestamp Options!")
}

func (d *DatadogSniffer) Start() {
	d.t.Go(d.Sniff)
}

func (d *DatadogSniffer) Stop() error {
	d.t.Kill(nil)
	return d.t.Wait()
}

//Unexported - we only call this ourselves.
func (d *DatadogSniffer) die(err error) {
	d.t.Kill(err)
}

func (d *DatadogSniffer) Running() bool {
	return d.t.Alive()
}

func (d *DatadogSniffer) SetPcapHandle(handle *pcap.Handle) {
	d.pcapHandle = handle
}

func (d *DatadogSniffer) handlePacket(data []byte, ci *gopacket.CaptureInfo) error {
	err := d.decoder.parser.DecodeLayers(data, &d.decoder.decoded)
	if err != nil {
		log.Infof("error decoding packet: %v", err)
		return err
	}
	// Find either the IPv4 or IPv6 address to use as our network
	// layer.
	foundNetLayer := false
	foundIPv4Layer := false
	for _, typ := range d.decoder.decoded {
		switch typ {
		case layers.LayerTypeIPv4:
			foundNetLayer = true
			foundIPv4Layer = true
		case layers.LayerTypeIPv6:
			foundNetLayer = true
		case layers.LayerTypeTCP:
			if foundNetLayer && foundIPv4Layer {
				//do we have this flow? Build key
				var src, dst string
				ourIP := d.hostIPs[d.decoder.ip4.SrcIP.String()]

				// consider us always the SRC (this will help us keep just one tag for
				// all comms between two ip's
				if ourIP {
					src = net.JoinHostPort(d.decoder.ip4.SrcIP.String(), strconv.Itoa(int(d.decoder.tcp.SrcPort)))
					dst = net.JoinHostPort(d.decoder.ip4.DstIP.String(), strconv.Itoa(int(d.decoder.tcp.DstPort)))
				} else {
					src = net.JoinHostPort(d.decoder.ip4.DstIP.String(), strconv.Itoa(int(d.decoder.tcp.DstPort)))
					dst = net.JoinHostPort(d.decoder.ip4.SrcIP.String(), strconv.Itoa(int(d.decoder.tcp.SrcPort)))
				}

				idle := time.Duration(d.IdleTTL * int(time.Second))
				flow, exists := d.flows.Get(src + "-" + dst)
				if exists == false {
					// TCPAccounting objects self-expire if they are inactive for a period of time >idle
					if ourIP {
						flow = NewTCPAccounting(d.decoder.ip4.SrcIP, d.decoder.ip4.DstIP, d.decoder.tcp.SrcPort, d.decoder.tcp.DstPort, idle, &d.flows.Expire)
					} else {
						flow = NewTCPAccounting(d.decoder.ip4.DstIP, d.decoder.ip4.SrcIP, d.decoder.tcp.DstPort, d.decoder.tcp.SrcPort, idle, &d.flows.Expire)
					}
					flow.Lock()
					d.flows.Add(src+"-"+dst, flow)
					flow.SetExpiration(idle, src+"-"+dst)
				} else {
					//flow still alive - reset timer
					flow.Lock()
					flow.Alive.Reset(idle)
				}

				if d.ExpTTL > 0 && d.decoder.tcp.ACK && d.decoder.tcp.FIN && !flow.Done {
					expTTL := time.Duration(d.ExpTTL * int(time.Second))

					// Here we clean up flows that have expired by the book - that is, we have seen
					// the TCP stream come to an end FIN/ACK and have kept these around so short-lived
					// flows actually get reported.

					//set timer
					flow.Done = true
					flow.SetExpiration(expTTL, src+"-"+dst)
				}

				tcp_payload_sz := uint32(d.decoder.ip4.Length) - uint32((d.decoder.ip4.IHL+d.decoder.tcp.DataOffset)*4)
				if ourIP && tcp_payload_sz > 0 {
					var t TCPKey
					//get the TS
					ts, _, _ := GetTimestamps(&d.decoder.tcp)
					t.TS = ts
					t.Seq = d.decoder.tcp.Seq

					//insert or update
					flow.Timed[t] = ci.Timestamp.UnixNano()

				} else if !ourIP {
					var t TCPKey
					//get the TS
					_, tsecr, _ := GetTimestamps(&d.decoder.tcp)
					t.TS = tsecr
					t.Seq = d.decoder.tcp.Ack

					if flow.Timed[t] != 0 {
						if flow.Seen[d.decoder.tcp.Ack] == false && d.decoder.tcp.ACK {
							//we can't receive an ACK for packet we haven't seen sent - we're the source
							rtt := uint64(ci.Timestamp.UnixNano() - flow.Timed[t])
							flow.CalcSRTT(rtt, d.Soften)
							flow.CalcJitter(rtt, d.Soften)
							flow.MaxRTT(rtt)
							flow.MinRTT(rtt)
							flow.Last = rtt
							flow.Sampled++
						}
						flow.Seen[d.decoder.tcp.Ack] = true
					}
				}
				flow.Unlock()
			}
		}
	}
	return nil
}

func (d *DatadogSniffer) SniffLive() {

	quit := false
	for !quit {

		// Although desirable we're currently unable to use the ZeroCopy method
		// for reading packet data. Unfortunately successive calls invalidate the
		// data slice we're operating on. Giving place to bad results.
		// Keep this in mind as a viable optimization for the future:
		//   - packet retrieval using  ZeroCopyReadPacketData.
		data, ci, err := d.pcapHandle.ReadPacketData()

		if err == nil {
			d.handlePacket(data, &ci)
		}
		select {
		case <-d.t.Dying():
			log.Infof("Done sniffing.")
			quit = true
		default:
			continue
		}
	}
}

func (d *DatadogSniffer) SniffOffline() {
	packetSource := gopacket.NewPacketSource(d.pcapHandle, d.pcapHandle.LinkType())

	for packet := range packetSource.Packets() {
		//Grab Packet CaptureInfo metadata
		ci := packet.Metadata().CaptureInfo
		d.handlePacket(packet.Data(), &ci)
		select {
		case <-d.t.Dying():
			log.Infof("Done sniffing.")
			break
		default:
			continue
		}
	}
}

func (d *DatadogSniffer) Sniff() error {

	if d.pcapHandle == nil {

		log.Infof("starting capture on interface %q", d.Iface)

		if d.Iface != fileInterface {
			// Set up pcap packet capture
			inactive, err := pcap.NewInactiveHandle(d.Iface)
			if err != nil {
				log.Errorf("Unable to create inactive handle for %q", d.Iface)
				d.reporter.Stop()
				d.die(err)
				return err
			}
			defer inactive.CleanUp()

			inactive.SetSnapLen(d.Snaplen)
			inactive.SetPromisc(false)
			inactive.SetTimeout(time.Second)

			// Maybe we should make the timestamp source selectable - Not all OS will allow that.
			//ts_sources := inactive.SupportedTimestamps()
			//for i := range ts_sources {
			//	log.Printf("TS source: %v:%v", ts_sources[i], ts_sources[i].String())
			//}

			handle, err := inactive.Activate()
			if err != nil {
				log.Errorf("Unable to activate %q", d.Iface)
				d.reporter.Stop()
				d.die(err)
				return err
			}
			d.pcapHandle = handle
		} else {
			handle, err := pcap.OpenOffline(d.config.Pcap)
			if err != nil {
				log.Errorf("Unable to open pcap file %q", d.config.Pcap)
				d.reporter.Stop()
				d.die(err)
				return err
			}
			d.pcapHandle = handle
		}
	}

	ifaces, err := pcap.FindAllDevs()
	if err != nil {
		log.Fatalf("Error getting interface details: %s", err)
	}

	ifaceFound := false
	ifaceDetails := make([]pcap.Interface, len(ifaces)-1)
	for i := range ifaces {
		if ifaces[i].Name == d.Iface {
			ifaceDetails[i] = ifaces[i]
			ifaceFound = true
		}
	}

	if !ifaceFound && d.Iface != fileInterface {
		log.Fatalf("Could not find interface details for: %s", d.Iface)
	}

	// we need to identify if we're the source/destination
	hosts := make([]string, 0)
	for i := range ifaceDetails {
		for j := range ifaceDetails[i].Addresses {
			ipStr := ifaceDetails[i].Addresses[j].IP.String()
			if strings.Contains(ipStr, "::") {
				log.Infof("IPv6 currently unsupported ignoring: %s", ipStr)
			} else {
				hosts = append(hosts, fmt.Sprintf("host %s", ipStr))
				d.hostIPs[ipStr] = true
			}
		}
	}
	for i := range d.config.Ips {
		hosts = append(hosts, fmt.Sprintf("host %s", d.config.Ips[i]))
	}

	bpfFilter := strings.Join(hosts, " or ")

	d.Filter += " and not host 127.0.0.1"
	if len(hosts) > 0 {
		d.Filter += " and " + " (" + bpfFilter + ")"
	}

	log.Infof("Setting BPF filter: %s", d.Filter)
	if err := d.pcapHandle.SetBPFFilter(d.Filter); err != nil {
		log.Fatalf("error setting BPF filter: %s", err)
	}

	log.Infof("reading in packets")
	if d.Iface == fileInterface {
		d.SniffOffline()
	} else {
		d.SniffLive()
	}

	for k := range d.flows.FlowMapKeyIterator() {
		flow, e := d.flows.Get(k)
		if e && flow.Sampled > 0 {
			if d.Soften {
				log.Infof("Flow %s\t w/ %d packets\tRTT:%6.2f ms", k, flow.Sampled, float64(int64(flow.SRTT)*int64(time.Nanosecond))/float64(time.Millisecond))
			} else {
				log.Infof("Flow %s\t w/ %d packets\tRTT:%6.2f ms", k, flow.Sampled, float64(flow.SRTT)*float64(time.Nanosecond)/float64(time.Millisecond))
			}
		}
	}

	//Shutdown reporter thread
	return d.reporter.Stop()
}
