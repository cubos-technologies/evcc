package server

import (
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/core/site"
	"github.com/evcc-io/evcc/util/config"
	"github.com/evcc-io/evcc/util/templates"
)

type siteRefs struct {
	Title   *string
	Grid    *string
	PV      *[]string
	Battery *[]string
	Aux     *[]string
	Ext     *[]string
}

func MQTTupdateRef(usage string, class templates.Class, name string, delete bool, site site.API) error {
	switch class {
	case templates.Charger:

	case templates.Meter:
		var newSiteRef siteRefs
		switch usage {
		case "pv":
			newSiteRef.PV = new([]string)
			*newSiteRef.PV = site.GetPVMeterRefs()
			if delete {
				for index, element := range *newSiteRef.PV {
					if element == name {
						slice := *newSiteRef.PV
						*newSiteRef.PV = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.PV = append(*newSiteRef.PV, name)
			}

		case "grid":
			newRef := ""
			if !delete {
				newRef = name
			}

			newSiteRef.Grid = &newRef
			MQTTupdateSiteHandler(newSiteRef, site)
			return nil
		case "battery":
			newSiteRef.Battery = new([]string)
			*newSiteRef.Battery = site.GetBatteryMeterRefs()
			if delete {
				for index, element := range *newSiteRef.Battery {
					if element == name {
						slice := *newSiteRef.Battery
						*newSiteRef.Battery = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.Battery = append(*newSiteRef.Battery, name)
			}
		case "aux":
			newSiteRef.Aux = new([]string)
			*newSiteRef.Aux = site.GetAuxMeterRefs()
			if delete {
				for index, element := range *newSiteRef.Aux {
					if element == name {
						slice := *newSiteRef.Aux
						*newSiteRef.Aux = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.Aux = append(*newSiteRef.Aux, name)
			}
		case "ext":
			newSiteRef.Ext = new([]string)
			*newSiteRef.Ext = site.GetExtMeterRefs()
			if delete {
				for index, element := range *newSiteRef.Ext {
					if element == name {
						slice := *newSiteRef.Ext
						*newSiteRef.Ext = append(slice[:index], slice[(index+1):]...)
					}
				}
			} else {
				*newSiteRef.Ext = append(*newSiteRef.Ext, name)
			}
		}

		if err := MQTTupdateSiteHandler(newSiteRef, site); err != nil {
			return err
		}

	case templates.Vehicle:

	case templates.Circuit:
	}
	return nil
}

func MQTTupdateSiteHandler(payload siteRefs, site site.API) error {
	if payload.Title != nil {
		site.SetTitle(*payload.Title)
	}

	if payload.Grid != nil {
		if *payload.Grid != "" {
			if err := MQTTvalidateRefs([]string{*payload.Grid}); err != nil {
				return err
			}
		}

		site.SetGridMeterRef(*payload.Grid)
		setConfigDirty()
	}

	if payload.PV != nil {
		if err := MQTTvalidateRefs(*payload.PV); err != nil {
			return err
		}

		site.SetPVMeterRefs(*payload.PV)
		setConfigDirty()
	}

	if payload.Battery != nil {
		if err := MQTTvalidateRefs(*payload.Battery); err != nil {
			return err
		}

		site.SetBatteryMeterRefs(*payload.Battery)
		setConfigDirty()
	}
	if payload.Ext != nil {
		if err := MQTTvalidateRefs(*payload.Ext); err != nil {
			return err
		}

		site.SetExtMeterRefs(*payload.Ext)
		setConfigDirty()
	}
	return nil
}

func MQTTvalidateRefs(refs []string) error {
	for _, m := range refs {
		if _, err := config.Meters().ByName(m); err != nil {
			return err
		}
	}
	return nil
}

func appendMeterToSite(meter api.Meter, usage string, ref string, site site.API) {
	switch usage {
	case "pv":
		site.AppendPVMeter(meter, ref)
	case "grid":
		site.AppendGridMeter(meter, ref)
	}
}

