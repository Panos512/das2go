// DAS mongo module
//
// Copyright (c) 2015-2016 - Valentin Kuznetsov <vkuznet AT gmail dot com>
//
// References : https://gist.github.com/boj/5412538
//              https://gist.github.com/border/3489566
package mongo

import (
	"encoding/json"
	"fmt"
	"github.com/vkuznet/das2go/config"
	"github.com/vkuznet/das2go/utils"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
	"log"
	"strings"
)

type DASRecord map[string]interface{}

func (r DASRecord) ToString() string {
	var out []string
	for _, k := range utils.MapKeys(r) {
		switch v := r[k].(type) {
		case int:
			out = append(out, fmt.Sprintf("%s:%d", k, v))
		case float64:
			d := int(v)
			if float64(d) == v {
				out = append(out, fmt.Sprintf("%s:%d", k, d))
			} else {
				out = append(out, fmt.Sprintf("%s:%f", k, v))
			}
		default:
			s := fmt.Sprintf("%s:%#v", k, r[k])
			out = append(out, strings.Replace(s, ", ", ",\n   ", -1))
		}
	}
	return strings.Join(out, "\n")
}

// function to get int value from DAS record for given key
func GetValue(rec DASRecord, key string) interface{} {
	var val DASRecord
	keys := strings.Split(key, ".")
	if len(keys) > 1 {
		switch v := rec[keys[0]].(type) {
		case DASRecord:
			val = v
		case []DASRecord:
			val = v[0]
		case []interface{}:
			vvv := v[0]
			val = vvv.(DASRecord)
		default:
			msg := fmt.Errorf("DAS ERROR GetValue unknown type=%T, data=%v", v, v)
			panic(msg)
		}
		if len(keys) == 2 {
			return GetValue(val, keys[1])
		}
		return GetValue(val, strings.Join(keys[1:len(keys)], "."))
	}
	value := rec[key]
	return value
}

// function to get string value from DAS record for given key
func GetStringValue(rec DASRecord, key string) (string, error) {
	value := GetValue(rec, key)
	val, ok := value.(string)
	if ok {
		return val, nil
	}
	return "", fmt.Errorf("Unable to cast value for key '%s'", key)
}

// function to get int value from DAS record for given key
func GetIntValue(rec DASRecord, key string) (int, error) {
	value := GetValue(rec, key)
	val, ok := value.(int)
	if ok {
		return val, nil
	}
	return 0, fmt.Errorf("Unable to cast value for key '%s'", key)
}

// function to get int value from DAS record for given key
func GetInt64Value(rec DASRecord, key string) (int64, error) {
	value := GetValue(rec, key)
	out, ok := value.(int64)
	if ok {
		return out, nil
	}
	return 0, fmt.Errorf("Unable to cast value for key '%s'", key)
}

type MongoConnection struct {
	Session *mgo.Session
}

func (m *MongoConnection) Connect() *mgo.Session {
	var err error
	if m.Session == nil {
		m.Session, err = mgo.Dial(config.Uri())
		if err != nil {
			panic(err)
		}
		//         m.Session.SetMode(mgo.Monotonic, true)
		//         m.Session.SetMode(mgo.Monotonic, true)
		m.Session.SetMode(mgo.Strong, true)
	}
	return m.Session.Clone()
}

// global object which holds MongoDB connection
var _Mongo MongoConnection

// insert into MongoDB
func Insert(dbname, collname string, records []DASRecord) {
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	for _, rec := range records {
		if err := c.Insert(&rec); err != nil {
			log.Println("Fail to insert DAS record", err)
		}
	}
}

// get records from MongoDB
func Get(dbname, collname string, spec bson.M, idx, limit int) []DASRecord {
	out := []DASRecord{}
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	var err error
	if limit > 0 {
		err = c.Find(spec).Skip(idx).Limit(limit).All(&out)
	} else {
		err = c.Find(spec).Skip(idx).All(&out)
	}
	if err != nil {
		panic(err)
	}
	return out
}

// get records from MongoDB sorted by given key
func GetSorted(dbname, collname string, spec bson.M, skeys []string) []DASRecord {
	out := []DASRecord{}
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	err := c.Find(spec).Sort(skeys...).All(&out)
	if err != nil {
		panic(err)
	}
	return out
}

// helper function to present in bson selected fields
func sel(q ...string) (r bson.M) {
	r = make(bson.M, len(q))
	for _, s := range q {
		r[s] = 1
	}
	return
}

// get records from MongoDB filtered and sorted by given key
func GetFilteredSorted(dbname, collname string, spec bson.M, fields, skeys []string, idx, limit int) []DASRecord {
	out := []DASRecord{}
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	var err error
	fields = append(fields, "das") // always extract das part of the record
	if limit > 0 {
		if len(skeys) > 0 {
			err = c.Find(spec).Skip(idx).Limit(limit).Select(sel(fields...)).Sort(skeys...).All(&out)
		} else {
			err = c.Find(spec).Skip(idx).Limit(limit).Select(sel(fields...)).All(&out)
		}
	} else {
		if len(skeys) > 0 {
			err = c.Find(spec).Select(sel(fields...)).Sort(skeys...).All(&out)
		} else {
			err = c.Find(spec).Select(sel(fields...)).All(&out)
		}
	}
	if err != nil {
		panic(err)
	}
	return out
}

// update inplace for given spec
func Update(dbname, collname string, spec, newdata bson.M) {
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	err := c.Update(spec, newdata)
	if err != nil {
		panic(err)
	}
}

// get number records from MongoDB
func Count(dbname, collname string, spec bson.M) int {
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	nrec, err := c.Find(spec).Count()
	if err != nil {
		panic(err)
	}
	return nrec
}

// remove records from MongoDB
func Remove(dbname, collname string, spec bson.M) {
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	_, err := c.RemoveAll(spec)
	if err != nil && err != mgo.ErrNotFound {
		panic(err)
	}
}

// Load json data stream from series of bytes
func LoadJsonData(data []byte) DASRecord {
	r := make(DASRecord)
	err := json.Unmarshal(data, &r)
	if err != nil {
		panic(err)
	}
	return r
}

// create DAS cache indexes
func CreateIndexes(dbname, collname string, keys []string) {
	s := _Mongo.Connect()
	defer s.Close()
	c := s.DB(dbname).C(collname)
	for _, key := range keys {
		index := mgo.Index{
			Key:        []string{key},
			Unique:     false,
			Background: true,
			//             Sparse:     true,
		}
		err := c.EnsureIndex(index)
		if err != nil {
			panic(err)
		}
	}
}