package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/miekg/dns"
)

// example.com matches example.com but not foo.example.com
// *.example.com matches foo.example.com and example.com
// take care about dots.
func domainMatches(domain string, pattern string) bool {
	// Add trailing dot to both domain and pattern to make matching easier
	if !strings.HasSuffix(pattern, ".") {
		pattern += "."
	}
	if !strings.HasSuffix(domain, ".") {
		domain += "."
	}

	if domain == pattern {
		return true
	}
	if "*."+domain == pattern {
		return true
	}
	if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(domain, pattern[1:]) {
		return true
	}
	return false
}

func (s *Service) domainAllowed(domain string) bool {
	for _, d := range s.config.AllowedDomains {
		if domainMatches(domain, d) {
			return true
		}
	}
	return false
}

func (s *Service) handleDNSQuery(m *dns.Msg) {
	for _, q := range m.Question {
		switch q.Qtype {
		case dns.TypeA:
			log.Printf("Query for %s\n", q.Name)
			if !s.domainAllowed(q.Name) {
				log.Printf("Domain %s is not allowed\n", q.Name)
				m.Rcode = dns.RcodeNameError
				return
			}

			ips, err := net.LookupHost(q.Name)
			if err != nil {
				log.Printf("Failed to lookup host: %v\n", err)
				m.Rcode = dns.RcodeServerFailure
				return
			}

			for _, ip := range ips {
				tryExec("nft", "add", "element", "inet", "bender", "allow", "{", ip, "}")

				rr, err := dns.NewRR(fmt.Sprintf("%s A %s", q.Name, ip))
				if err != nil {
					log.Printf("Failed to create RR: %v\n", err)
					// ignore
				} else {
					m.Answer = append(m.Answer, rr)
				}
			}
		}
	}
}

func (s *Service) handleDNSRequest(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Compress = false

	switch r.Opcode {
	case dns.OpcodeQuery:
		s.handleDNSQuery(m)
	}

	w.WriteMsg(m)
}

func (s *Service) netRun() {
	s.setupNftables()

	// attach request handler func
	dns.HandleFunc(".", s.handleDNSRequest)

	// start DNS server
	server := &dns.Server{Addr: "127.0.0.93:53", Net: "udp"}
	log.Printf("Starting DNS server at %s", server.Addr)
	err := server.ListenAndServe()
	defer server.Shutdown()
	if err != nil {
		log.Fatalf("Failed to start DNS server: %s\n ", err.Error())
	}
}

func (s *Service) setupNftables() {
	c := exec.Command("nft", "-f", "-")
	c.Stdin = strings.NewReader(`
		table inet bender 
		delete table inet bender
		
		table inet bender {
			set allow {
				type ipv4_addr
				elements = { 127.0.0.93 }
			}
		
			chain output {
				type filter hook output priority 0; policy accept;
				socket cgroupv2 level 1 "bender" goto bender-output
			}
		
			chain bender-output {
				ip daddr @allow accept
				ip protocol tcp reject with tcp reset
				reject with icmp type host-prohibited
			}
		}
	`)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()
	if err != nil {
		log.Fatalf("Failed to setup nftables: %v", err)
	}

}
