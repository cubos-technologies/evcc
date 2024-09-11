package cubosdata

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/evcc-io/evcc/api/globalconfig"
	"github.com/evcc-io/evcc/provider/mqtt"
	"github.com/evcc-io/evcc/util"
)

func CubosMqttProdConfig() (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := getGatewaySerial()

	caCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/ca.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientKey, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.key"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:     "tls://mqtt.mycubos.com:8883",
			User:       "live",
			Password:   "9y6t`,/$Z5jlsJ\\HsOgeK$/el2(Vy~&e",
			ClientID:   serial + "-evcc",
			Insecure:   true,
			CaCert:     caCert,
			ClientCert: clientCert,
			ClientKey:  clientKey,
		},
		Topic: "Blackbox/" + serial,
	}, nil
}

func CubosMqttDevConfig() (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := getGatewaySerial()

	caCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/ca.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientCert, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.crt"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	clientKey, err = func(b []byte, err error) (string, error) {
		return string(b), err
	}(os.ReadFile(".cubos_data/client.key"))
	if err != nil {
		return globalconfig.Mqtt{}, err
	}

	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:     "tls://mqtt.mycubos.com:8884",
			User:       "live",
			Password:   "9y6t`,/$Z5jlsJ\\HsOgeK$/el2(Vy~&e",
			ClientID:   serial + "-evcc",
			Insecure:   true,
			CaCert:     caCert,
			ClientCert: clientCert,
			ClientKey:  clientKey,
		},
		Topic: "Blackbox/" + serial,
	}, nil
}

func CubosMqttLocalConfig() (globalconfig.Mqtt, error) {
	serial := "GW00000000"
	return globalconfig.Mqtt{
		Config: mqtt.Config{
			Broker:   "host.docker.internal:1883",
			ClientID: serial + "-evcc",
		},
		Topic: "Blackbox/" + serial,
	}, nil
}

func getGatewaySerial() string {
	var serial string
	log := util.NewLogger("eeprom")

	//TODO: rework
	cmd := exec.Command("sh", "-c", `/app/eepromDumper /sys/bus/i2c/devices/2-0050/eeprom | grep -Eo 'caSerialNumber\s+GW[0-9]{8}' | sed 's/.*GW/GW/' | tr -d '\n'`)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	serial = strings.TrimSpace(stdout.String())
	if err != nil || serial == "" {
		log.WARN.Println("failed to get serial, using GW00000000")
		return "GW00000000"
	} else {
		if matched, err := regexp.MatchString(`^GW\d{8}$`, serial); err != nil && matched {
			log.INFO.Printf("got gateway serial %s", serial)
		} else {
			log.WARN.Printf("serial %s does not match regex, using GW00000000\n", serial)
			return "GW00000000"
		}
	}
	return serial
}
