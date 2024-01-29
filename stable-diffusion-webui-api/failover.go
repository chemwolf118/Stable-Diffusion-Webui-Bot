package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zijiren233/stable-diffusion-webui-bot/gconfig"
	"github.com/zijiren233/stable-diffusion-webui-bot/utils"

	"github.com/zijiren233/go-colorlog"
)

// var loadBalance = &api{
// 	apiList: &[]*apiUrl{},
// 	lock:    &sync.RWMutex{},
// }

func (api *API) GetWoker() (m *sync.Map, c chan *apiUrl, a []*apiUrl) {
	api.loadBalance.lock.RLock()
	defer api.loadBalance.lock.RUnlock()
	if api.loadBalance.working != nil {
		m = *api.loadBalance.working
	}
	if api.loadBalance.apiPool != nil {
		c = *api.loadBalance.apiPool
	}
	if api.loadBalance.apiList != nil {
		a = *api.loadBalance.apiList
	}
	return
}

type api struct {
	apiList *[]*apiUrl
	apiPool *chan *apiUrl
	working **sync.Map // api -> bool
	lock    *sync.RWMutex
}

var backendOnce = &sync.Once{}

func (api *API) Load(apis []gconfig.Api) {
	api.loadAPI(apis)
	backendOnce.Do(func() {
		go api.back()
		go api.failover()
	})
}

type apiUrl struct {
	gconfig.Api
	Models       []Model
	CurrentModel string
	LoadedModels *sync.Map
	a            *API
}

func (api *apiUrl) ChangeOption(config *Config) error {
	option := map[string]interface{}{"add_version_to_infotext": false, "lora_add_hashes_to_infotext": false, "add_model_hash_to_info": false, "add_model_name_to_info": true, "deepbooru_use_spaces": true, "interrogate_clip_dict_limit": 0, "interrogate_return_ranks": true, "deepbooru_sort_alpha": false, "interrogate_deepbooru_score_threshold": 0.5, "interrogate_clip_min_length": 15, "interrogate_clip_max_length": 50, "live_previews_enable": false, "sd_checkpoint_cache": 0, "sd_vae_checkpoint_cache": 0, "grid_save": false, "eta_noise_seed_delta": 31337, "eta_ancestral": 1, "samples_save": false, "enable_emphasis": true}
	if config != nil {
		if config.Model != "" {
			option["sd_model_checkpoint"] = config.Model
		}
		if config.Vae != "" {
			option["sd_vae"] = config.Vae
		}
		if config.ClipSkip != 0 {
			option["CLIP_stop_at_last_layers"] = config.ClipSkip
		}
	}
	b, err := json.Marshal(option)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, api.GenerateApi("options"), bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth(api.Username, api.Password)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if string(body) == "null" {
		if config != nil && config.Model != "" {
			api.CurrentModel = config.Model
			api.LoadedModels.Store(config.Model, nil)
		}
		return nil
	}
	return fmt.Errorf("change option err: %s", string(body))
}

func (a *API) next(tryModel string) (*apiUrl, func()) {
	a.getApiL.Lock()
	defer a.getApiL.Unlock()
	var (
		working           *sync.Map
		apiChan           chan *apiUrl
		currentApiChanPtr chan *apiUrl
		api               *apiUrl
		firstApi          *apiUrl
		ok                bool
		count             = 0
	)
	for {
		working, apiChan, _ = a.GetWoker()
		if len(apiChan) == 0 {
			continue
		}
		if currentApiChanPtr == nil {
			currentApiChanPtr = apiChan
		} else if currentApiChanPtr != apiChan {
			currentApiChanPtr = apiChan
			firstApi = nil
			count = 0
		}
		api, ok = <-apiChan
		if !ok {
			continue
		}
		if v, ok := working.Load(api.Url); ok && v.(bool) {
			if tryModel == "" || api.CurrentModel == tryModel || func() bool {
				if count == 0 {
					return false
				}
				_, ok := api.LoadedModels.Load(tryModel)
				return ok
			}() {
				break
			}
			if firstApi == nil {
				firstApi = api
				count = 0
			} else if api == firstApi {
				count++
				if count == 2 {
					break
				}
			}
		}
		apiChan <- api
	}
	down := new(sync.Once)
	return api, func() {
		down.Do(func() {
			apiChan <- api
		})
	}
}

func (api *apiUrl) GenerateApi(types string) string {
	if strings.HasPrefix(types, "/") {
		return fmt.Sprint(api.Url, types)
	}
	return fmt.Sprint(api.Url, "/sdapi/v1/", types)
}

func (a *API) loadAPI(apis []gconfig.Api) {
	a.loadBalance.lock.Lock()
	defer a.loadBalance.lock.Unlock()
	working := &sync.Map{}
	apiPool := make(chan *apiUrl, len(apis))
	api := []*apiUrl{}
	for _, v := range apis {
		working.Store(v, false)
		apiU := apiUrl{Api: v, LoadedModels: &sync.Map{}, a: a}
		apiPool <- &apiU
		api = append(api, &apiU)
	}
	if SliceEqualBCE(api, *a.loadBalance.apiList) {
		return
	}
	a.loadBalance.apiList = &api
	a.loadBalance.apiPool = &apiPool
	a.loadBalance.working = &working
}

func SliceEqualBCE(a, b []*apiUrl) bool {
	if len(a) != len(b) {
		return false
	}

	if (a == nil) != (b == nil) {
		return false
	}

	for i, v := range a {
		if v.Url != b[i].Url {
			return false
		}
	}

	return true
}

var errReturn = errors.New("detail return error")
var errModels = errors.New("not allow models")

type Model struct {
	ModelName string `json:"model_name"`
}

func (a *API) failover() {
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	wait := &sync.WaitGroup{}
	var workers uint32
	for range timer.C {
		working, _, apiList := a.GetWoker()
		for _, v := range apiList {
			wait.Add(1)
			go func(api *apiUrl) {
				defer wait.Done()
				err := utils.Retry(2, false, 0, func() (bool, error) {
					ctx, cf := context.WithTimeout(context.Background(), time.Second*3)
					defer cf()
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, api.GenerateApi("sd-models"), nil)
					if err != nil {
						return false, err
					}
					req.SetBasicAuth(api.Username, api.Password)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						return true, err
					}
					defer resp.Body.Close()
					b, err := io.ReadAll(resp.Body)
					if err != nil {
						return false, err
					}
					if json.Unmarshal(b, &api.Models) != nil {
						return false, errReturn
					}
					if !a.ModelAllowed(api.Models) {
						return false, errModels
					}
					return false, nil
				})
				if err != nil {
					colorlog.Errorf("api [%s] have some error: %v", api.Url, err)
					working.Store(api.Url, false)
					api.LoadedModels.Range(func(key, value any) bool {
						api.LoadedModels.Delete(key)
						return true
					})
				} else {
					if v, ok := working.Load(api); ok && !v.(bool) {
						api.CurrentModel = a.models[0]
					}
					api.LoadedModels.Store(a.models[0], nil)
					working.Store(api.Url, true)
					atomic.AddUint32(&workers, 1)
				}
			}(v)
		}
		wait.Wait()
		if workers == 0 {
			workers = 1
		}
		a.drawPool.Tune(int(workers))
		workers = 0
		timer.Reset(time.Second * 10)
	}
}

func (a *API) ModelAllowed(Model []Model) bool {
	if len(a.models) > len(Model) {
		return false
	}
	for _, v := range a.models {
		for k, model := range Model {
			if v == model.ModelName {
				break
			}
			if k == len(Model)-1 {
				return false
			}
		}
	}
	return true
}
