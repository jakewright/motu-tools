package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

const (
	// Network address of the Motu interface
	motuAddress = "192.168.88.251"

	// How many steps between min and max
	volumeDenominations = 16

	// The type of scale used by the property
	scaleLinear = "linear"
	scaleLog    = "log"

	volumeSound = "/System/Library/LoginPlugins/BezelServices.loginPlugin/Contents/Resources/volume.aiff"
)

type Device struct {
	// The property that controls the gain of this property
	Property string

	// The property that controls whether this device is muted
	MuteProperty string

	// Type of scale (linear or logarithmic)
	Scale string

	// Allowed range of values.
	// If scale is log, these are values in dB (as displayed in the MOTU UI).
	Max float64
	Min float64

	// Once Min is reached, we skip straight to zero volume.
	// If scale is log, this is NOT dB but instead the amplitude ratio value
	ZeroVolume float64
}

var devices = map[string]*Device{
	"main": {
		Property:     "datastore/ext/obank/1/ch/0/stereoTrim",
		MuteProperty: "datastore/mix/main/0/matrix/mute", // 0.0 (unmuted) or 1.0 (muted)
		Scale:        scaleLinear,
		Max:          0,
		Min:          -50,
		ZeroVolume:   -127,
	},
	"computer": {
		Property:     "datastore/mix/chan/10/matrix/fader",
		MuteProperty: "datastore/mix/chan/10/matrix/mute",
		Scale:        scaleLog,
		Max:          0,
		Min:          -64,
		ZeroVolume:   0,
	},
}

const (
	deviceMain   = "main"
	devicePhones = "phones"

	motuPropertyPhonesTrim = "datastore/ext/obank/0/ch/0/stereoTrim"
	motuPropertyMainTrim   = "datastore/ext/obank/1/ch/0/stereoTrim"
	//motuPropertyFaderMain  = "datastore/mix/main/0/matrix/fader"

	phonesTrimMin = "-127"
	phonesTrimMax = "-20"
	mainTrimMin   = "-127"
	mainTrimMax   = "-20"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Not enough arguments\n")
		os.Exit(1)
	}

	m, err := NewFromIPAddress(motuAddress)
	if err != nil {
		fmt.Printf("Failed to create client: %v\n", err)
		os.Exit(1)
	}

	d, ok := devices[os.Args[1]]
	if !ok {
		fmt.Printf("Unknown device: %s\n", os.Args[1])
		os.Exit(1)
	}

	switch os.Args[2] {

	case "mute":
		err = m.Mute(d)
	case "inc", "increment":
		err = m.IncDec(d, true)
	case "dec", "decrement":
		err = m.IncDec(d, false)
	default:
		fmt.Printf("Unrecongised command: %s\n", os.Args[1])
		os.Exit(1)
	}

	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}
}

type MotuClient struct {
	MOTUAddress *url.URL
	HTTPClient  *http.Client
}

func NewFromIPAddress(ip string) (*MotuClient, error) {
	addr, err := url.Parse(fmt.Sprintf("http://%s", ip))
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	return &MotuClient{
		MOTUAddress: addr,
		HTTPClient:  http.DefaultClient,
	}, nil
}

//func (m *MotuClient) Mute() error {
//	if err := m.patch(motuPropertyFaderMain, 0); err != nil {
//
//	}
//}

func (m *MotuClient) Mute(d *Device) error {
	current, err := m.get(d.MuteProperty)
	if err != nil {
		return fmt.Errorf("failed to get current value: %w", err)
	}

	var newValue float64 = 0
	switch current {
	case 0:
		newValue = 1
	case 1: // Ok
	default:
		return fmt.Errorf("unexpected current mute value: %f", current)
	}

	if err := m.patch(d.MuteProperty, newValue); err != nil {
		return fmt.Errorf("failed to update property: %w", err)
	}

	return nil
}

func (m *MotuClient) IncDec(d *Device, inc bool) error {
	current, err := m.get(d.Property)
	if err != nil {
		return fmt.Errorf("failed to get current value: %w", err)
	}

	var newValue float64
	switch d.Scale {
	case scaleLinear:
		newValue = m.newVolumeLinear(d, current, inc)
	case scaleLog:
		newValue = m.newVolumeLog(d, current, inc)
	default:
		panic("unknown scale")
	}

	if err := m.patch(d.Property, newValue); err != nil {
		return fmt.Errorf("failed to update property: %w", err)
	}

	if err := playSound(); err != nil {
		return fmt.Errorf("failed to play sound: %w", err)
	}

	return nil
}

func (m *MotuClient) newVolumeLinear(d *Device, current float64, inc bool) float64 {
	delta := (d.Max - d.Min) / volumeDenominations

	var newVolume float64
	if inc {
		newVolume = math.Ceil(current) + delta
	} else {
		newVolume = math.Ceil(current) - delta
	}

	// Go straight to mute once we reach min volume to avoid the
	// range of volumes being skewed towards the barely-audible range
	if !inc && newVolume <= d.Min {
		return d.ZeroVolume
	}

	// Keep the volume within the bounds
	return math.Min(math.Max(newVolume, d.Min), d.Max)
}

func (m *MotuClient) newVolumeLog(d *Device, current float64, inc bool) float64 {
	// Convert the amplitude ratio value to a decibel value
	// https://en.wikipedia.org/wiki/Decibel
	currentDB := 10 * math.Log10(math.Pow(current, 2))

	delta := (d.Max - d.Min) / volumeDenominations

	var newDB float64
	if inc {
		newDB = math.Ceil(currentDB) + delta
	} else {
		newDB = math.Ceil(currentDB) - delta
	}

	// Go straight to mute once we reach min volume to avoid the
	// range of volumes being skewed towards the barely-audible range
	if !inc && newDB <= d.Min {
		if d.ZeroVolume != 0 {
			panic("logarithmic zero volume should be zero")
		}
		return d.ZeroVolume
	}

	// Keep the volume within the bounds
	newDB = math.Min(math.Max(newDB, d.Min), d.Max)

	// Convert back to amplitude ratio and bound to [0, 1]
	newAmpRatio := math.Sqrt(math.Pow(10, newDB/10))
	return math.Min(math.Max(newAmpRatio, 0), 1)
}

func (m *MotuClient) get(property string) (float64, error) {
	rsp, err := m.HTTPClient.Get(m.MOTUAddress.JoinPath(property).String())
	if err != nil {
		return 0, fmt.Errorf("failed to get property value: %w", err)
	}

	defer rsp.Body.Close()

	body, err := io.ReadAll(rsp.Body)
	if err != nil {
		return 0, fmt.Errorf("failed to read body: %w", err)
	}

	type wrapper struct {
		Value float64 `json:"value"`
	}

	parsed := wrapper{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return parsed.Value, nil
}

func (m *MotuClient) patch(property string, value float64) error {
	// The API is cursed and wants the value to be formatted as JSON
	// under the key "value", and then form-encoded.
	form := url.Values{}
	form.Add("json", fmt.Sprintf(`{"value": %f}`, value))

	req, err := http.NewRequest(
		http.MethodPatch,
		m.MOTUAddress.JoinPath(property).String(),
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	rsp, err := m.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to make request: %w", err)
	}

	defer rsp.Body.Close()
	return nil
}

func playSound() error {
	if err := exec.Command("afplay", volumeSound).Run(); err != nil {
		return fmt.Errorf("failed to run afplay: %w", err)
	}

	return nil
}
