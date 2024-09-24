package tariff

import (
	"slices"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/evcc-io/evcc/api"
	"github.com/evcc-io/evcc/tariff/cuboslsw"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/request"
)

type CubosLsw struct {
	*embed
	log  *util.Logger
	uri  string
	data *util.Monitor[api.Rates]
}

var _ api.Tariff = (*CubosLsw)(nil)

func init() {
	registry.Add("cuboslsw", NewCubosLswFromConfig)
}

func NewCubosLswFromConfig(other map[string]interface{}) (api.Tariff, error) {
	cc := struct {
		embed `mapstructure:",squash"`
	}{}

	if err := util.DecodeOther(other, &cc); err != nil {
		return nil, err
	}

	t := &CubosLsw{
		embed: &cc.embed,
		log:   util.NewLogger("cuboslsw"),
		uri:   cuboslsw.URI,
		data:  util.NewMonitor[api.Rates](time.Hour),
	}

	done := make(chan error)
	go t.run(done)
	err := <-done

	return t, err
}

func (t *CubosLsw) run(done chan error) {
	var once sync.Once

	client := request.NewHelper(t.log)

	tick := time.NewTicker(time.Hour)
	for ; true; <-tick.C {
		var res cuboslsw.Prices

		if err := backoff.Retry(func() error {
			return backoffPermanentError(client.GetJSON(t.uri, &res))
		}, bo()); err != nil {
			once.Do(func() { done <- err })

			t.log.ERROR.Println(err)
			continue
		}

		data := make(api.Rates, 0, len(res.Data))
		for _, r := range res.Data {
			ar := api.Rate{
				Start: r.StartTimestamp.Local(),
				End:   r.EndTimestamp.Local(),
				// TODO: check price divider?
				Price: t.totalPrice(r.Marketprice / 100),
			}
			data = append(data, ar)
		}

		mergeRates(t.data, data)
		once.Do(func() { close(done) })
	}
}

func (t *CubosLsw) Rates() (api.Rates, error) {
	var res api.Rates
	err := t.data.GetFunc(func(val api.Rates) {
		res = slices.Clone(val)
	})
	return res, err
}

func (t *CubosLsw) Type() api.TariffType {
	return api.TariffTypePriceDynamic
}
