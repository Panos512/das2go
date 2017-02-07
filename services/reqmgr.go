package services

// DAS service module
// ReqMgr module
//
// Copyright (c) 2015-2016 - Valentin Kuznetsov <vkuznet AT gmail dot com>
//

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/vkuznet/das2go/dasql"
	"github.com/vkuznet/das2go/mongo"
	"github.com/vkuznet/das2go/utils"
)

// helper function to load ReqMgr data stream
func loadReqMgrData(api string, data []byte) []mongo.DASRecord {
	var out []mongo.DASRecord
	if api == "configIDs" || api == "datasetByPrepID" || api == "outputdataset" || api == "inputdataset" {
		var rec mongo.DASRecord
		// to prevent json.Unmarshal behavior to convert all numbers to float
		// we'll use json decode method with instructions to use numbers as is
		buf := bytes.NewBuffer(data)
		dec := json.NewDecoder(buf)
		dec.UseNumber()
		err := dec.Decode(&rec)

		// original way to decode data
		// err := json.Unmarshal(data, &rec)
		if err != nil {
			msg := fmt.Sprintf("ReqMgr unable to unmarshal the data into DAS record, api=%s, data=%s, error=%v", api, string(data), err)
			out = append(out, mongo.DASErrorRecord(msg))
		}
		out = append(out, rec)
	} else if api == "recentDatasetByPrepID" {
		var datasets []string
		err := json.Unmarshal(data, &datasets)
		if err != nil {
			msg := fmt.Sprintf("ReqMgr unable to unmarshal the data into DAS record, api=%s, data=%s, error=%v", api, string(data), err)
			out = append(out, mongo.DASErrorRecord(msg))
		}
		for _, d := range datasets {
			rec := make(mongo.DASRecord)
			rec["name"] = d
			out = append(out, rec)
		}
	} else {
		err := json.Unmarshal(data, &out)
		if err != nil {
			msg := fmt.Sprintf("ReqMgr unable to unmarshal the data into DAS record, api=%s, data=%s, error=%v", api, string(data), err)
			out = append(out, mongo.DASErrorRecord(msg))
		}
	}
	return out
}

// ReqMgrUnmarshal unmarshals ReqMgr data stream and return DAS records based on api
func ReqMgrUnmarshal(api string, data []byte) []mongo.DASRecord {
	records := loadReqMgrData(api, data)
	var out []mongo.DASRecord
	if api == "inputdataset" {
		for _, rec := range records {
			val := rec["InputDatasets"]
			if val != nil {
				datasets := val.([]string)
				rec["name"] = datasets[0]
			}
			out = append(out, rec)
		}
		return out
	} else if api == "outputdataset" {
		for _, rec := range records {
			val := rec["OutputDatasets"]
			if val != nil {
				datasets := val.([]string)
				rec["name"] = datasets[0]
			}
			out = append(out, rec)
		}
		return out
	} else if api == "configIDs" {
		for _, rec := range records {
			for key, val := range rec {
				crec := make(mongo.DASRecord)
				crec["request_name"] = key
				crec["config_files"] = val
				out = append(out, crec)
			}
		}
		return out
	} else if api == "datasetByPrepID" {
		for _, rec := range records {
			for _, v := range rec {
				crec := make(mongo.DASRecord)
				crec["name"] = v
				out = append(out, crec)
			}
		}
		return out
	}
	return records
}

/*
 * LOCAL APIs
 */

// helper function to find ReqMgr ids
func findReqMgrIds(base, dataset string) ([]string, map[string][]string) {
	var out, urls []string
	var rurl string
	exit := false
	rurl = fmt.Sprintf("%s/couchdb/reqmgr_workload_cache/_design/ReqMgr/_view/byoutputdataset?key=\"%s\"&include_docs=true&stale=update_after", base, dataset)
	urls = append(urls, rurl)
	rurl = fmt.Sprintf("%s/couchdb/reqmgr_workload_cache/_design/ReqMgr/_view/byinputdataset?key=\"%s\"&include_docs=true&stale=update_after", base, dataset)
	urls = append(urls, rurl)
	rurl = fmt.Sprintf("%s/couchdb/wmstats/_design/WMStats/_view/requestByOutputDataset?key=\"%s\"&include_docs=true&stale=update_after", base, dataset)
	urls = append(urls, rurl)
	rurl = fmt.Sprintf("%s/couchdb/wmstats/_design/WMStats/_view/requestByInputDataset?key=\"%s\"&include_docs=true&stale=update_after", base, dataset)
	urls = append(urls, rurl)
	ch := make(chan utils.ResponseType)
	defer close(ch)
	idict := make(map[string][]string)
	umap := map[string]int{}
	for _, u := range urls {
		umap[u] = 1 // keep track of processed urls below
		go utils.Fetch(u, "", ch)
	}
	for {
		select {
		case r := <-ch:
			var data mongo.DASRecord
			err := json.Unmarshal(r.Data, &data)
			if err == nil {
				values := data["rows"]
				if values != nil {
					rows := values.([]interface{})
					for _, rec := range rows {
						row := rec.(map[string]interface{})
						out = append(out, row["id"].(string))
						doc := row["doc"].(map[string]interface{})
						val := doc["ProcConfigCacheID"]
						if val != nil {
							out = append(out, val.(string))
						}
						val = doc["ConfigCacheID"]
						if val != nil {
							out = append(out, val.(string))
						}
						val = doc["SkimConfigCacheID"]
						if val != nil {
							out = append(out, val.(string))
						}
					}
				}
			}
			idict[r.Url] = out
			delete(umap, r.Url) // remove Url from map
		default:
			if len(umap) == 0 { // no more requests, merge data records
				exit = true
			}
			time.Sleep(time.Duration(10) * time.Millisecond) // wait for response
		}
		if exit {
			break
		}
	}
	return utils.List2Set(out), idict
}

// L_reqmgr2_configs reqmgr APIs to lookup configs for given dataset
// The logic: we look-up ReqMgr ids for given dataset and scan them
// if id has length 32 we use configFile URL, otherwise we look-up record
// in couchdb and fetch ConfigIDs to construct configFile URL
func (LocalAPIs) L_reqmgr2_configs(dasquery dasql.DASQuery) []mongo.DASRecord {
	return reqmgrConfigs(dasquery)
}

// L_reqmgr_configs reqmgr APIs
func (LocalAPIs) L_reqmgr_configs(dasquery dasql.DASQuery) []mongo.DASRecord {
	return reqmgrConfigs(dasquery)
}

func reqmgrConfigs(dasquery dasql.DASQuery) []mongo.DASRecord {
	spec := dasquery.Spec
	base := "https://cmsweb.cern.ch"
	// find ReqMgr Ids for given dataset
	dataset := spec["dataset"].(string)
	ids, idict := findReqMgrIds(base, dataset)
	var urls, rurls []string
	var rurl string
	for _, v := range ids {
		if len(v) == 32 {
			rurl = fmt.Sprintf("%s/couchdb/reqmgr_workload_cache/%s/configFile", base, v)
			urls = append(urls, rurl)
		} else {
			rurl = fmt.Sprintf("%s/couchdb/reqmgr_workload_cache/%s", base, v)
			rurls = append(rurls, rurl)
		}
	}

	// if we have reqmgr urls we must resolve it they lead to actual config files
	umap := map[string]int{}
	exit := false
	ch := make(chan utils.ResponseType)
	defer close(ch)
	for _, u := range rurls {
		umap[u] = 1 // keep track of processed urls below
		go utils.Fetch(u, "", ch)
	}
	for {
		select {
		case r := <-ch:
			var data mongo.DASRecord
			err := json.Unmarshal(r.Data, &data)
			configIDs := []string{"DQMConfigCacheID", "ConfigCacheID"}
			if err == nil {
				for _, attr := range configIDs {
					val := data[attr]
					switch v := val.(type) {
					case string:
						rurl = fmt.Sprintf("%s/couchdb/reqmgr_config_cache/%s/configFile", base, v)
						urls = append(urls, rurl)
					case []string:
						for _, u := range v {
							rurl = fmt.Sprintf("%s/couchdb/reqmgr_config_cache/%s/configFile", base, u)
							urls = append(urls, rurl)
						}
					}
				}
				// look for configs in tasks
				for _, key := range utils.MapKeys(data) {
					if strings.HasPrefix(key, "Task") {
						rec := data[key]
						var vvv map[string]interface{}
						switch r := rec.(type) {
						case map[string]interface{}:
							vvv = r
						default:
							continue
						}
						for _, attr := range configIDs {
							val := vvv[attr]
							if val != nil {
								switch v := val.(type) {
								case string:
									rurl = fmt.Sprintf("%s/couchdb/reqmgr_config_cache/%s/configFile", base, v)
									urls = append(urls, rurl)
								case []string:
									for _, u := range v {
										rurl = fmt.Sprintf("%s/couchdb/reqmgr_config_cache/%s/configFile", base, u)
										urls = append(urls, rurl)
									}
								}
							}
						}
					}
				}
			}
			delete(umap, r.Url) // remove Url from map
		default:
			if len(umap) == 0 { // no more requests, merge data records
				exit = true
			}
			time.Sleep(time.Duration(10) * time.Millisecond) // wait for response
		}
		if exit {
			break
		}
	}

	// Construct final record
	rec := make(mongo.DASRecord)
	rec["dataset"] = dataset
	rec["name"] = "ReqMgr/WMStats"
	rec["urls"] = mongo.DASRecord{"output": urls}
	rec["ids"] = ids
	rec["idict"] = idict
	var out []mongo.DASRecord
	out = append(out, rec)
	return out
}
