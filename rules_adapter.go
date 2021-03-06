// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	kingpin "gopkg.in/alecthomas/kingpin.v2"
	redis "gopkg.in/redis.v4"
	yaml "gopkg.in/yaml.v2"

	"github.com/prometheus/common/model"
	"github.com/prometheus/common/version"
	"github.com/tinytub/rules_adapter/pkg/logkit"
	"github.com/tinytub/rules_adapter/pkg/rulefmt"
)

// TODO: 打点
var logger log.Logger

func main() {

	logLevel := logkit.AllowedLevel{}
	logLevel.Set("info")
	logger = logkit.New(logLevel)

	app := kingpin.New(filepath.Base(os.Args[0]), "Tooling for the Prometheus rule generate.")
	app.Version(version.Print("rule adapter"))
	app.HelpFlag.Short('h')

	updateCmd := app.Command("update", "Update the resources to newer formats.")
	ruleFilePath := updateCmd.Arg("path", "rules file path").Required().ExistingDir()
	redisPath := updateCmd.Arg("redis", "redis path ip:port").Required().TCP()
	redisPassword := updateCmd.Arg("password", "redis path password").Required().String()

	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case updateCmd.FullCommand():
		RefreshRules(*ruleFilePath, (*redisPath).String(), *redisPassword)
	}

}

type judgeRecored struct {
	Name string `json:"alarm_name"`
	Expr string `json:"expre"`
	Step int    `json:"step"`
}

func RefreshRules(path, redis, password string) {

	interval := time.Duration(3 * time.Second)
	updateRules(path, redis, password)

	for {
		select {
		case <-time.Tick(interval):
			updateRules(path, redis, password)
		}
	}

}

func updateRules(fpath, redis, password string) {
	//TODO: filename with job or service name
	filename := "wonder"
	abpath := path.Join(fpath, filename+".yml")
	absfpath, _ := filepath.Abs(abpath)

	data, err := getRedisData(redis, password)

	if err != nil {
		level.Error(logger).Log("msg", "get data from redis error", "err", err)
		return
	}

	remoteRuleGroups, err := getRemoteRules(data)
	if err != nil {
		level.Error(logger).Log("msg", "get Remote rules error", "err", err)
		return
	}

	//check rules
	_, localRuleGroups, errsLocal := checkLocalRules(absfpath)

	//TODO: 文件不存在的时候该如何处理？
	if errsLocal != nil {
		level.Error(logger).Log("msg", "local rules err", "err", errsLocal)
		return
	}

	updates := checkUpdate(*localRuleGroups, *remoteRuleGroups)

	if updates > 0 {

		y, err := yaml.Marshal(*remoteRuleGroups)
		if err != nil {
			level.Error(logger).Log("msg", "yaml marshal error", "err", err)
			return
		}

		updateRulesFile(y, absfpath)
		reloadPromeConfig()
	}

}

func getRedisData(path, password string) ([]string, error) {
	client := redis.NewClient(&redis.Options{
		Network:  "tcp",
		Addr:     path,
		Password: password,
		DB:       0,
	})
	defer client.Close()

	data, err := client.LRange("CUSTOM_EXPRESS_STRATEGY", 0, -1).Result()
	if err != nil {
		return nil, err
	}

	return data, nil
}

func getRemoteRules(data []string) (*rulefmt.RuleGroups, error) {

	allRules := make(map[int][]rulefmt.Rule)

	for _, d := range data {
		var record judgeRecored
		err := json.Unmarshal([]byte(d), &record)
		if err != nil {
			continue
		}
		rule := rulefmt.Rule{
			Record: record.Name,
			Expr:   record.Expr,
		}
		if err := rule.Validate(); err != nil {
			level.Error(logger).Log("msg", "badRule", "err", fmt.Sprint(err))
			continue
		}
		allRules[record.Step] = append(allRules[record.Step], rule)
	}

	var groups []rulefmt.RuleGroup

	for step, rules := range allRules {
		group := rulefmt.RuleGroup{
			Name:     "wonder" + strconv.Itoa(step) + "Group",
			Interval: model.Duration(time.Duration(step) * time.Second),
		}
		group.Rules = rules
		groups = append(groups, group)
	}

	remoteRulesMap := &rulefmt.RuleGroups{Groups: groups}

	return remoteRulesMap, nil
}

func checkLocalRules(filename string) (int, *rulefmt.RuleGroups, []error) {
	//fmt.Println("Checking", filename)
	level.Debug(logger).Log("checking", filename)
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		f, _ := os.OpenFile(filename, os.O_RDONLY|os.O_CREATE, 0666)
		f.Close()
	}
	rgs, errs := rulefmt.ParseFile(filename)
	if errs != nil {
		return 0, rgs, errs
	}

	numRules := 0
	for _, rg := range rgs.Groups {
		numRules += len(rg.Rules)
	}

	return numRules, rgs, nil
}

func checkUpdate(localRuleGroups, remoteRuleGroups rulefmt.RuleGroups) int {

	//startTime := time.Now()
	var newRules []string
	var newUpdates []string
	var deletedRules []string

	var nRule bool
	remoteRules := make(map[string][]rulefmt.Rule)
	localRules := make(map[string][]rulefmt.Rule)
	deletedMap := make(map[string]bool)

	for _, lRules := range localRuleGroups.Groups {
		localRules[lRules.Name] = lRules.Rules
		for _, rule := range lRules.Rules {
			deletedMap[rule.Record] = true
		}
	}

	for _, rRules := range remoteRuleGroups.Groups {
		remoteRules[rRules.Name] = rRules.Rules
	}

	for name, rRules := range remoteRules {
		for _, rRule := range rRules {
			nRule = true
			for _, lRule := range localRules[name] {

				if lRule.Record == rRule.Record {
					deletedMap[lRule.Record] = false

					nRule = false
					if !reflect.DeepEqual(lRule, rRule) {
						newUpdates = append(newUpdates, rRule.Record)
						level.Info(logger).Log("newUpdate", rRule.Record)
						break
					}
					break
				}
			}
			if nRule {
				newRules = append(newRules, rRule.Record)
				level.Info(logger).Log("newRules", rRule.Record)
			}
		}
	}

	for record, v := range deletedMap {
		if v == true {
			deletedRules = append(deletedRules, record)
			level.Info(logger).Log("deletedRule", record)
		}
	}

	updates := len(newRules) + len(newUpdates) + len(deletedRules)

	return updates
}

func updateRulesFile(data []byte, filename string) {
	if err := ioutil.WriteFile(filename, data, 0666); err != nil {
		level.Error(logger).Log("msg", "write file error", "err", err)
	}
}

func reloadPromeConfig() {

	_, err := exec.Command("sh", "-c", "pkill -SIGHUP prometheus").Output()
	if err != nil {
		level.Error(logger).Log("msg", "prometheus reload Failed")
		return
	}
	level.Info(logger).Log("msg", "prometheus reloaded")
}
