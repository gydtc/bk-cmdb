/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package ccapi

import (
	"configcenter/src/common"
	"configcenter/src/common/blog"
	"configcenter/src/common/core/cc/api"
	"configcenter/src/common/core/cc/config"
	"configcenter/src/common/errors"
	"configcenter/src/common/http/httpserver"
	"configcenter/src/common/metric"
	"configcenter/src/common/types"
	confCenter "configcenter/src/source_controller/proccontroller/procdata/config"
	"configcenter/src/source_controller/proccontroller/procdata/rdiscover"
	"configcenter/src/storage"
	"github.com/emicklei/go-restful"
	"sync"
	"time"
)

//CCAPIServer define data struct of bcs ccapi server
type CCAPIServer struct {
	conf     *config.CCAPIConfig
	httpServ *httpserver.HttpServer
	rd       *rdiscover.RegDiscover
	cfCenter *confCenter.ConfCenter
}

func NewCCAPIServer(conf *config.CCAPIConfig) (*CCAPIServer, error) {
	s := &CCAPIServer{}

	//config
	s.conf = conf
	addr, _ := s.conf.GetAddress()
	port, _ := s.conf.GetPort()

	//http server
	s.httpServ = httpserver.NewHttpServer(port, addr, "")

	a := api.NewAPIResource()
	a.SetConfig(s.conf)
	a.InitAction()

	// RDiscover
	s.rd = rdiscover.NewRegDiscover(s.conf.RegDiscover, addr, port, false)
	// ConfCenter
	s.cfCenter = confCenter.NewConfCenter(s.conf.RegDiscover)

	return s, nil
}

//Stop the ccapi server
func (ccAPI *CCAPIServer) Stop() error {
	return nil
}

//Start the ccapi server
func (ccAPI *CCAPIServer) Start() error {
	chErr := make(chan error, 3)

	a := api.NewAPIResource()

	// configure center
	go func() {
		err := ccAPI.cfCenter.Start()
		blog.Errorf("configure center module start failed!. err:%s", err.Error())
		chErr <- err
	}()

	/// fetch config of itselft
	var confData []byte
	var config map[string]string
	for {
		// temp code, just to debug
		if ccAPI.conf.ExConfig != "" {
			config, _ = a.ParseConfig()
			break
		}
		// end temp code
		confData = ccAPI.cfCenter.GetConfigureCxt()
		if len(chErr) > 0 {
			return <-chErr
		}
		if confData == nil {
			blog.Warnf("fail to get configure, will get again")
			time.Sleep(time.Second * 2)
			continue
		} else {
			blog.Infof("get configure. ctx(%s)", string(confData))
			config, _ = a.ParseConf(confData)
			break
		}
	}

	// load the errors resource
	if errorres, ok := config["errors.res"]; ok {
		if errif, err := errors.New(errorres); nil != err {
			blog.Error("failed to create errors object, error info is  %s ", err.Error())
			chErr <- err
		} else {
			a.Error = errif
		}
	} else {
		for {
			errcode := ccAPI.cfCenter.GetErrorCxt()
			if errcode == nil {
				blog.Warnf("fail to get language package, will get again")
				time.Sleep(time.Second * 2)
				continue
			} else {
				errif := errors.NewFromCtx(errcode)
				a.Error = errif
				blog.Info("lanugage package loaded")
				break
			}
		}
	}

	wg := sync.WaitGroup{}

	wg.Add(1)
	go func() {
		wg.Done()
		err := a.GetDataCli(config, "mongodb")
		if err != nil {
			blog.Error("connect mongodb error exit! err:%s", err.Error())
			chErr <- err
		}
	}()

	wg.Add(1)
	go func() {

		err := a.GetDataCli(config, storage.DI_REDIS)
		if err != nil {
			blog.Error("connect redis error exit! err:%s", err.Error())
			chErr <- err
		}
		wg.Done()
	}()

	go func() {
		err := ccAPI.rd.Start()
		blog.Errorf("rdiscover start failed! err:%s", err.Error())
	}()

	//http server
	go func() {
		wg.Wait()
		ccAPI.initHttpServ()
		err := ccAPI.httpServ.ListenAndServe()
		blog.Error("http listen and serve failed! err:%s", err.Error())
		chErr <- err
	}()

	select {
	case err := <-chErr:
		blog.Error("exit! err:%s", err.Error())
		return err
	}

}

func (ccAPI *CCAPIServer) initHttpServ() error {
	a := api.NewAPIResource()

	ccAPI.httpServ.RegisterWebServer("/process/{version}", nil, a.Actions)
	// MetricServer
	conf := metric.Config{
		ModuleName:    types.CC_MODULE_PROCCONTROLLER,
		ServerAddress: ccAPI.conf.AddrPort,
	}
	metricActions := metric.NewMetricController(conf, ccAPI.HealthMetric)
	as := []*httpserver.Action{}
	for _, metricAction := range metricActions {
		as = append(as, &httpserver.Action{Verb: common.HTTPSelectGet, Path: metricAction.Path, Handler: func(req *restful.Request, resp *restful.Response) {
			metricAction.HandlerFunc(resp.ResponseWriter, req.Request)
		}})
	}
	ccAPI.httpServ.RegisterWebServer("/", nil, as)

	return nil
}

// HealthMetric check netservice is health
func (ccAPI *CCAPIServer) HealthMetric() metric.HealthMeta {

	meta := metric.HealthMeta{IsHealthy: true}
	a := api.GetAPIResource()

	// check mongo
	meta.Items = append(meta.Items, metric.NewHealthItem("mongo", a.InstCli.Ping()))
	// check redis
	meta.Items = append(meta.Items, metric.NewHealthItem("redis", a.CacheCli.Ping()))
	// check zk
	meta.Items = append(meta.Items, metric.NewHealthItem(types.CCFunctionalityServicediscover, ccAPI.rd.Ping()))

	for _, item := range meta.Items {
		if item.IsHealthy == false {
			meta.IsHealthy = false
			meta.Message = "proccontroller is not healthy"
			break
		}
	}

	return meta
}
