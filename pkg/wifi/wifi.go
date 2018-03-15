// Copyright 2018 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package wifi

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/u-root/u-root/pkg/wpa/passphrase"
)

const (
	nopassphrase = `network={
		ssid="%s"
		proto=RSN
		key_mgmt=NONE
	}`
	eap = `network={
		ssid="%s"
		key_mgmt=WPA-EAP
		identity="%s"
		password="%s"
	}`
)

var (
	// RegEx for parsing iwlist output
	cellRE       = regexp.MustCompile("(?m)^\\s*Cell")
	essidRE      = regexp.MustCompile("(?m)^\\s*ESSID.*")
	encKeyOptRE  = regexp.MustCompile("(?m)^\\s*Encryption key:(on|off)$")
	wpa2RE       = regexp.MustCompile("(?m)^\\s*IE: IEEE 802.11i/WPA2 Version 1$")
	authSuitesRE = regexp.MustCompile("(?m)^\\s*Authentication Suites .*$")

	// RegEx for parsing iwconfig output
	iwconfigRE = regexp.MustCompile("(?m)^[a-zA-Z0-9]+\\s*IEEE 802.11.*$")
)

type SecProto int

const (
	NoEnc SecProto = iota
	WpaPsk
	WpaEap
	NotSupportedProto
)

type WifiOption struct {
	Essid     string
	AuthSuite SecProto
}

type WiFi interface {
	ScanInterfaces() ([]string, error)
	ScanWifi() ([]WifiOption, error)
	Connect(a ...string) error
}

type WiFiService struct {
	Interface string
}

func (w WiFiService) ScanInterfaces() ([]string, error) {
	o, err := exec.Command("iwconfig").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("iwconfig: %v (%v)", string(o), err)
	}
	return parseIwconfig(o), nil
}

func parseIwconfig(o []byte) (res []string) {
	interfaces := iwconfigRE.FindAll(o, -1)
	for _, i := range interfaces {
		res = append(res, strings.Split(string(i), " ")[0])
	}
	return
}

func (w WiFiService) ScanWifi() ([]WifiOption, error) {
	o, err := exec.Command("iwlist", w.Interface, "scanning").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("iwlist: %v (%v)", string(o), err)
	}
	return parseIwlistOut(o), nil
}

/*
 * Assumptions:
 *	1) Cell, essid, and encryption key option are 1:1 match
 *	2) We only support IEEE 802.11i/WPA2 Version 1
 *	3) Each WiFi only support (1) authentication suites (based on observations)
 */

func parseIwlistOut(o []byte) []WifiOption {
	cells := cellRE.FindAllIndex(o, -1)
	essids := essidRE.FindAll(o, -1)
	encKeyOpts := encKeyOptRE.FindAll(o, -1)

	if cells == nil {
		return nil
	}

	var res []WifiOption
	knownEssids := make(map[string]bool)

	// Assemble all the WiFi options
	for i := 0; i < len(cells); i++ {
		essid := strings.Trim(strings.Split(string(essids[i]), ":")[1], "\"\n")
		if knownEssids[essid] {
			continue
		}
		knownEssids[essid] = true
		encKeyOpt := strings.Trim(strings.Split(string(encKeyOpts[i]), ":")[1], "\n")
		if encKeyOpt == "off" {
			res = append(res, WifiOption{essid, NoEnc})
			continue
		}
		// Find the proper Authentication Suites
		start, end := cells[i][0], len(o)
		if i != len(cells)-1 {
			end = cells[i+1][0]
		}
		// Narrow down the scope when looking for WPA Tag
		wpa2SearchArea := o[start:end]
		l := wpa2RE.FindIndex(wpa2SearchArea)
		if l == nil {
			res = append(res, WifiOption{essid, NotSupportedProto})
			continue
		}
		// Narrow down the scope when looking for Authorization Suites
		authSearchArea := wpa2SearchArea[l[0]:]
		authSuites := strings.Trim(strings.Split(string(authSuitesRE.Find(authSearchArea)), ":")[1], "\n ")
		switch authSuites {
		case "PSK":
			res = append(res, WifiOption{essid, WpaPsk})
		case "802.1x":
			res = append(res, WifiOption{essid, WpaEap})
		default:
			res = append(res, WifiOption{essid, NotSupportedProto})
		}
	}
	return res
}

func (w WiFiService) Connect(a ...string) error {
	// format of a: [essid, pass, id]
	conf, err := generateConfig(a...)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile("/tmp/wifi.conf", conf, 0444); err != nil {
		return fmt.Errorf("/tmp/wifi.conf: %v", err)
	}

	c := make(chan error, 2)

	// There's no telling how long the supplicant will take, but on the other hand,
	// it's been almost instantaneous. But, further, it needs to keep running.
	go func() {
		cmd := exec.Command("wpa_supplicant", "-i"+w.Interface, "-c/tmp/wifi.conf")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr //For an easier time debugging
		if err := cmd.Run(); err != nil {
			c <- fmt.Errorf("wpa_supplicant error: %v", err)
		} else {
			c <- nil
		}
	}()

	go func() {
		cmd := exec.Command("dhclient", "-ipv4=true", "-ipv6=false", "-verbose", w.Interface)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr //For an easier time debugging
		if err := cmd.Run(); err != nil {
			c <- fmt.Errorf("dhclient error: %v", err)
		} else {
			c <- nil
		}
	}()

	if errWpaSupplicant, errDhClient := <-c, <-c; errWpaSupplicant != nil || errDhClient != nil {
		return fmt.Errorf("%v \n %v", errWpaSupplicant, errDhClient)
	}
	return nil
}

func generateConfig(a ...string) (conf []byte, err error) {
	// format of a: [essid, pass, id]
	switch {
	case len(a) == 3:
		conf = []byte(fmt.Sprintf(eap, a[0], a[2], a[1]))
	case len(a) == 2:
		conf, err = passphrase.Run(a[0], a[1])
		if err != nil {
			return nil, fmt.Errorf("essid: %v, pass: %v : %v", a[0], a[1], err)
		}
	case len(a) == 1:
		conf = []byte(fmt.Sprintf(nopassphrase, a[0]))
	default:
		return nil, fmt.Errorf("generateConfig needs 1, 2, or 3 args")
	}
	return
}