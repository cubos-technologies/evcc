package cubosdata

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"

	"github.com/evcc-io/evcc/api/globalconfig"
	"github.com/evcc-io/evcc/provider/mqtt"
	"github.com/evcc-io/evcc/util"
)

func CubosMqttConfig() (globalconfig.Mqtt, error) {
	log := util.NewLogger("cubos-config")
	//return CubosMqttLocalConfig(log)
	//return CubosMqttDevConfig(log)
	return CubosMqttProdConfig(log)
}

func CubosMqttProdConfig(log *util.Logger) (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := getGatewaySerial()
	log.INFO.Printf("using serial %s", serial)

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

func CubosMqttDevConfig(log *util.Logger) (globalconfig.Mqtt, error) {
	var err error
	var caCert, clientCert, clientKey string
	serial := getGatewaySerial()
	log.INFO.Printf("using serial %s", serial)

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

func CubosMqttLocalConfig(log util.Logger) (globalconfig.Mqtt, error) {
	serial := "GW00000000"
	log.INFO.Printf("using serial %s", serial)
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
	defaultSerial := "GW00000000"
	serialPattern := `^GW\d{8}$`
	log := util.NewLogger("eeprom")

	var eepromOut bytes.Buffer
	eepromCmd := exec.Command("/app/eepromDumper", "/sys/bus/i2c/devices/2-0050/eeprom")
	eepromCmd.Stdout = &eepromOut
	err := eepromCmd.Run()
	if err != nil {
		log.WARN.Printf("failed to run serial dumper: %w", err)
		return defaultSerial
	}

	var grepOut bytes.Buffer
	grepCmd := exec.Command("grep", "-Eo", "caSerialNumber\\s+GW[0-9]{8}")
	grepCmd.Stdin = &eepromOut
	grepCmd.Stdout = &grepOut
	err = grepCmd.Run()
	if err != nil {
		log.WARN.Printf("failed to run grep: %w", err)
		return defaultSerial
	}

	var sedOut bytes.Buffer
	sedCmd := exec.Command("sed", "s/.*GW/GW/")
	sedCmd.Stdin = &grepOut
	sedCmd.Stdout = &sedOut
	err = sedCmd.Run()
	if err != nil {
		log.WARN.Printf("failed to run sed: %w", err)
		return defaultSerial
	}

	var trOut bytes.Buffer
	trCmd := exec.Command("tr", "-d", "\n")
	trCmd.Stdin = &sedOut
	trCmd.Stdout = &trOut
	err = trCmd.Run()
	if err != nil {
		log.WARN.Printf("failed to run tr: %w", err)
		return defaultSerial
	}

	re, err := regexp.Compile(serialPattern)
	if err != nil {
		log.ERROR.Printf("failed to compile regex: %w")
		return defaultSerial
	}
	if !re.MatchString(serial) {
		log.WARN.Printf("serial %s does not match regex", serial)
		return defaultSerial
	}

	return serial
}
