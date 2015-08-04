/*
 *
 * Author     : Valentin Kuznetsov <vkuznet AT gmail dot com>
 * Description: Services module
 * Created    : Fri Jun 26 14:25:01 EDT 2015
 *
 */
package services

import (
	"dasmaps"
	"dasql"
	"gopkg.in/mgo.v2/bson"
	"mongo"
	"strings"
	"time"
	"utils"
)

// remap function uses DAS notations and convert series of DAS records
// into another set where appropriate remapping is done
func remap(api string, records []mongo.DASRecord, notations []mongo.DASRecord) []mongo.DASRecord {
	var out []mongo.DASRecord
	if len(records) == 0 {
		return records
	}
	keys := utils.MapKeys(records[0])
	for _, rec := range records {
		for _, row := range notations {
			apiname, api_key, rec_key := dasmaps.GetNotation(row)
			if apiname != "" {
				if apiname == api && utils.InList(api_key, keys) {
					rec[rec_key] = rec[api_key]
					delete(rec, api_key)
				}
			} else {
				if utils.InList(api_key, keys) {
					rec[rec_key] = rec[api_key]
				}
			}
		}
		out = append(out, rec)
	}
	return out
}

func Unmarshal(system, api string, data []byte, notations []mongo.DASRecord) []mongo.DASRecord {
	var out []mongo.DASRecord
	switch {
	case system == "phedex":
		out = PhedexUnmarshal(api, data)
	case system == "dbs3":
		out = DBSUnmarshal(api, data)
	}
	return remap(api, out, notations)
}

func DASHeader() mongo.DASRecord {
	das := make(mongo.DASRecord)
	das["expire"] = 60 // default expire
	das["record"] = 1  // by default it is a data record (1 vs das record 0)
	das["primary_key"] = ""
	das["instance"] = ""
	//     das["api"] = []string{}
	//     das["system"] = []string{}
	das["services"] = []string{}
	return das

}

// adjust DAS record and add (if necessary) leading key from DAS query
func AdjustRecords(dasquery dasql.DASQuery, system, api string, records []mongo.DASRecord, expire int, pkeys []string) []mongo.DASRecord {
	var out []mongo.DASRecord
	fields := dasquery.Fields
	qhash := dasquery.Qhash
	if len(fields) > 1 {
		return records
	}
	skey := fields[0]
	for _, rec := range records {
		// DAS header for records
		dasheader := DASHeader()
		srvs := dasheader["services"].([]string)
		srv := strings.Join([]string{system, api}, ":")
		srvs = append(srvs, srv)
		dasheader["services"] = srvs
		dasheader["expire"] = utils.Expire(expire)
		dasheader["primary_key"] = pkeys[0]

		keys := utils.MapKeys(rec)
		if utils.InList(skey, keys) {
			rec["qhash"] = qhash
			rec["das"] = dasheader
			out = append(out, rec)
		} else {
			newrec := make(mongo.DASRecord)
			newrec[skey] = rec
			newrec["qhash"] = qhash
			newrec["das"] = dasheader
			out = append(out, newrec)
		}
	}
	return out
}

// create DAS record for DAS cache
func CreateDASRecord(dasquery dasql.DASQuery, srvs, pkeys []string) mongo.DASRecord {
	dasrecord := make(mongo.DASRecord)
	dasrecord["query"] = dasquery.Query
	dasrecord["qhash"] = dasquery.Qhash
	dasrecord["instance"] = dasquery.Instance
	dasheader := DASHeader()
	dasheader["record"] = 0           // DAS record type, zero for DAS record
	dasheader["status"] = "requested" // initial status
	dasheader["services"] = srvs
	dasheader["primary_key"] = pkeys[0]
	dasheader["expire"] = utils.Expire(60) // initial expire, 60 seconds from now
	dasheader["ts"] = time.Now().Unix()
	dasrecord["das"] = dasheader
	return dasrecord
}

// get DAS record from das cache
func GetDASRecord(dasquery dasql.DASQuery) mongo.DASRecord {
	spec := bson.M{"qhash": dasquery.Qhash, "das.record": 0}
	rec := mongo.Get("das", "cache", spec, 0, 1)
	return rec[0]
}

// update DAS record in das cache
func UpdateDASRecord(qhash string, dasrecord mongo.DASRecord) {
	spec := bson.M{"qhash": qhash, "das.record": 0}
	newdata := bson.M{"query": dasrecord["query"], "qhash": dasrecord["qhash"], "instance": dasrecord["instance"], "das": dasrecord["das"]}
	mongo.Update("das", "cache", spec, newdata)
}

// helper function to get expire value from DAS/data record
func GetExpire(rec mongo.DASRecord) int64 {
	das := rec["das"].(mongo.DASRecord)
	expire := das["expire"].(int64)
	return expire
}

// merge DAS data records
func MergeDASRecords(dasquery dasql.DASQuery) ([]mongo.DASRecord, int64) {
	// get DAS record and extract primary key
	spec := bson.M{"qhash": dasquery.Qhash, "das.record": 0}
	records := mongo.Get("das", "cache", spec, 0, 1)
	dasrecord := records[0]
	das := dasrecord["das"].(mongo.DASRecord)
	pkey := das["primary_key"].(string)
	mkey := strings.Split(pkey, ".")[0]
	// get DAS data record sorted by primary key
	spec = bson.M{"qhash": dasquery.Qhash, "das.record": 1}
	var skeys []string
	skeys = append(skeys, pkey)
	records = mongo.GetSorted("das", "cache", spec, skeys)

	// loop over data records and merge them, extract smallest expire timestamp
	var expire int64
	expire = time.Now().Unix() * 2
	var out []mongo.DASRecord
	var oldrec, newrec, rec mongo.DASRecord
	oldrec = records[0] // init with first record
	newrec = records[0] // init with first record
	for _, rec = range records {
		das := rec["das"].(mongo.DASRecord)
		dasexpire := das["expire"].(int64)
		if expire > dasexpire {
			expire = dasexpire
		}
		data1, err1 := mongo.GetStringValue(oldrec, pkey)
		data2, err2 := mongo.GetStringValue(rec, pkey)
		if err1 == nil && err2 == nil && data1 != data2 {
			oldrec = rec
			out = append(out, newrec)
		} else {
			newrec = mergeRecords(rec, oldrec, mkey, dasquery.Qhash)
		}
	}
	// we still left with last oldrec which should be merged with last record from the loop
	newrec = mergeRecords(rec, oldrec, mkey, dasquery.Qhash)
	out = append(out, newrec)
	return out, expire
}

// function to merge DAS data records on given key
func mergeRecords(oldrec, newrec mongo.DASRecord, key, qhash string) mongo.DASRecord {
	var rec []mongo.DASRecord
	rec = append(rec, oldrec[key].(mongo.DASRecord))
	rec = append(rec, newrec[key].(mongo.DASRecord))
	das := mergeDASparts(oldrec["das"].(mongo.DASRecord), newrec["das"].(mongo.DASRecord))
	return mongo.DASRecord{key: rec, "qhash": qhash, "das": das}
}

// helper function to merge das parts of DAS records
func mergeDASparts(das1, das2 mongo.DASRecord) mongo.DASRecord {
	das := make(mongo.DASRecord)
	var srvs []string
	srvs1 := das1["services"].([]interface{})
	srvs2 := das2["services"].([]interface{})
	for _, srv := range srvs1 {
		srvs = append(srvs, srv.(string))
	}
	for _, srv := range srvs2 {
		srvs = append(srvs, srv.(string))
	}
	das["services"] = srvs
	var expire int64
	expire = time.Now().Unix() * 2
	ex1, err1 := mongo.GetInt64Value(das1, "expire")
	ex2, err2 := mongo.GetInt64Value(das1, "expire")
	if err1 == nil && ex1 < expire {
		expire = ex1
	}
	if err2 == nil && ex2 < expire {
		expire = ex2
	}
	das["expire"] = expire
	das["status"] = "ok" // merged step should return ok status
	das["primary_key"] = das1["primary_key"]
	das["record"] = 1
	return das
}

// helper function to fix all DAS cache record expire timestamps
func UpdateExpire(qhash string, records []mongo.DASRecord, dasexpire int64) []mongo.DASRecord {
	var out []mongo.DASRecord
	for _, rec := range records {
		das := rec["das"].(mongo.DASRecord)
		das["expire"] = dasexpire
		rec["das"] = das
		out = append(out, rec)
	}
	return out
}
