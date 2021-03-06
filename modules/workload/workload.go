package workload

// workload api objects to represent resources in RMD

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/intel/rmd/internal/plugins"
	proxyclient "github.com/intel/rmd/internal/proxy/client"
	"github.com/intel/rmd/modules/cache"
	cacheconf "github.com/intel/rmd/modules/cache/config"
	"github.com/intel/rmd/utils/cpu"
	"github.com/intel/rmd/utils/pqos"
	"github.com/intel/rmd/utils/resctrl"

	libutil "github.com/intel/rmd/utils/bitmap"
	"github.com/intel/rmd/utils/proc"

	"github.com/intel/rmd/internal/db"
	rmderror "github.com/intel/rmd/internal/error"
	"github.com/intel/rmd/modules/policy"
	wltypes "github.com/intel/rmd/modules/workload/types"
	util "github.com/intel/rmd/utils"
	appConf "github.com/intel/rmd/utils/config"
)

var l sync.Mutex

// database for storing all active workloads
var workloadDatabase db.DB

// Flag to check if MBA and L3 CAT is supported
var isMbaSupported, isL3CATSupported, isMbaMbpsAvailable bool

var mbaMaxValue, mbaValue uint32

// reusable function for filling workload with policy-based params
func fillWorkloadByPolicy(wrkld *wltypes.RDTWorkLoad) error {
	if wrkld == nil {
		return fmt.Errorf("Invalid workload pointer")
	}
	if len(wrkld.Policy) == 0 {
		return fmt.Errorf("No policy in provided workload object")
	}

	// workload contains policy description - try to set all params
	policy, err := policy.GetDefaultPolicy(wrkld.Policy)
	if err != nil {
		return fmt.Errorf("Could not find the Policy. %v", err)
	}

	// cache allocation is not mandatory so use param if they exists
	var errMax error
	var errMin error

	// check and copy cache data
	maxCache, ok := policy["cache"]["max"].(int64)
	if !ok {
		errMax = fmt.Errorf("Failed to convert type for max cache")
	}

	minCache, ok := policy["cache"]["min"].(int64)
	if !ok {
		errMin = fmt.Errorf("Failed to convert type for min cache")
	}

	if errMax == nil && errMin == nil {
		wrkld.Rdt.Cache.Max = new(uint32)
		*wrkld.Rdt.Cache.Max = uint32(maxCache)
		wrkld.Rdt.Cache.Min = new(uint32)
		*wrkld.Rdt.Cache.Min = uint32(minCache)
	}

	if (wrkld.Rdt.Cache.Max != nil && wrkld.Rdt.Cache.Min == nil) || (wrkld.Rdt.Cache.Max == nil && wrkld.Rdt.Cache.Min != nil) {
		return fmt.Errorf("Invalid policy - exactly one *Cache param defined")
	}

	// check and copy MBA data
	valMba, ok := policy["mba"]["percentage"].(int)
	if ok {
		wrkld.Rdt.Mba.Percentage = new(uint32)
		*wrkld.Rdt.Mba.Percentage = uint32(valMba)
	}

	// get data from policy and fill plugins' params
	for mod, data := range policy {
		log.Debugf("Params for module %v found in policy", mod)
		// "cache" and "mba" are currently internal part of workload - not plugins
		if mod == "cache" || mod == "mba" {
			continue
		}
		// check and create Plugins only if there's a need to put something inside
		if wrkld.Plugins == nil {
			wrkld.Plugins = make(map[string]map[string]interface{})
		}
		// copy plugin params into workload
		wrkld.Plugins[mod] = data
	}

	return nil
}

// validate the request workload object is validated.
func validate(w *wltypes.RDTWorkLoad) error {
	if len(w.TaskIDs) <= 0 && len(w.CoreIDs) <= 0 {
		return fmt.Errorf("No task or core id specified")
	}

	// Firstly verify the task.
	ps := proc.ListProcesses()
	for _, task := range w.TaskIDs {
		if _, ok := ps[task]; !ok {
			return fmt.Errorf("The task: %s does not exist", task)
		}
	}

	if w.Policy == "" {
		// MBA part
		// there have to be both cache values or none of them
		if (w.Rdt.Cache.Max == nil && w.Rdt.Cache.Min != nil) || (w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min == nil) {
			return fmt.Errorf("Need to provide both cache.* or none of them")
		}
		// If MBA values are provided :
		// 1. Check if its a Cache guaranteed request
		// 2. Check if MBA value is range of 1 to max
		// 3. If any of the above fails then return error
		if isMbaSupported {
			if isMbaMbpsAvailable {
				if w.Rdt.Mba.Percentage != nil {
					return fmt.Errorf("Please provide MBA in Mbps")
				}
				if w.Rdt.Mba.Mbps != nil {
					mbaValue = *w.Rdt.Mba.Mbps
				}
			} else {
				if w.Rdt.Mba.Mbps != nil {
					return fmt.Errorf("Please provide MBA in Percentage")
				}
				if w.Rdt.Mba.Percentage != nil {
					mbaValue = *w.Rdt.Mba.Percentage
				}
			}
		}

		if w.Rdt.Mba.Mbps != nil || w.Rdt.Mba.Percentage != nil {
			if w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min != nil &&
				((*w.Rdt.Cache.Max != *w.Rdt.Cache.Min && mbaValue != mbaMaxValue) ||
					(*w.Rdt.Cache.Min == 0 && *w.Rdt.Cache.Max == 0 && mbaValue != mbaMaxValue)) {
				return fmt.Errorf("MBA only supported for Guaranteed Request and not for BestEffort and Shared")
			}
			if mbaValue > mbaMaxValue || mbaValue <= 0 {
				return fmt.Errorf("MBA values in should range from 1 to %d", mbaMaxValue)
			}
		}

		if isL3CATSupported && isMbaSupported {
			if w.Rdt.Cache.Max == nil && w.Rdt.Cache.Min == nil && (w.Rdt.Mba.Percentage != nil || w.Rdt.Mba.Mbps != nil) {
				return fmt.Errorf("Need to provide both cache and mba for better performance")
			}
		} else {
			if isL3CATSupported {
				if w.Rdt.Mba.Percentage != nil || w.Rdt.Mba.Mbps != nil {
					return fmt.Errorf("This machine supports only cache and not MBA")
				}
			} else {
				if w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min != nil {
					return fmt.Errorf("This machine supports only MBA and not cache")
				}
			}
		}

		// Plugins part
		// call Validate() for each loaded module defined in this workload
		for module, params := range w.Plugins {
			log.Debugf("Validating params for %v module", module) // temporary log

			// if params changed fetch module (if exists)
			pluginIface, ok := plugins.Interfaces[module]
			if !ok {
				// module not loaded but requested
				return rmderror.NewAppError(http.StatusBadRequest, "Trying to use module that is not loaded")
			}
			if pluginIface == nil {
				// module seems to be loaded but interface is nil
				return rmderror.NewAppError(http.StatusInternalServerError, "Error when processing loaded modules")
			}
			// unify param types coming from JSON
			paramsMap, err := util.UnifyMapParamsTypes(params)
			if err != nil {
				return err
			}
			// add core ids and process ids to params
			valInts, err := prepareCoreIDs(w.CoreIDs)
			if err != nil {
				return rmderror.NewAppError(http.StatusBadRequest, "Invalid params (core ids) received")
			}
			paramsMap["CPUS"] = valInts
			valInts, err = prepareCoreIDs(w.TaskIDs)
			if err != nil {
				return rmderror.NewAppError(http.StatusBadRequest, "Invalid params (task ids) received")
			}
			paramsMap["TASKS"] = valInts

			// and validate params
			err = pluginIface.Validate(paramsMap)
			if err != nil {
				return rmderror.NewAppError(http.StatusBadRequest, "Invalid params received", err)
			}
		}
	} else {
		// if policy is defined then all params should be overwritten by defaults
		err := fillWorkloadByPolicy(w)
		log.Infof("Policy overwritten workload params: %v", w)
		// finish here (with or without error)
		return err
	}

	// at least one of following params must be provided:
	// - policy (checked above)
	// - RDT.Cache or RDT.Mba
	// - other loadable plugins params

	if w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min != nil {
		// Cache params defined
		return nil
	}

	if w.Rdt.Mba.Percentage != nil {
		// MBA params defined
		return nil
	}

	if len(w.Plugins) > 0 {
		// params exists and are validated above - so workload should be fine
		return nil
	}

	// if reached this point then something went wrong
	return fmt.Errorf("No RDT/Plugins params in workload")
}

func enforceCache(w *wltypes.RDTWorkLoad, er *wltypes.EnforceRequest, rdtenforce *wltypes.RDTEnforce) error {
	resaall := proxyclient.GetResAssociation(pqos.GetAvailableCLOSes())

	// log.Println("Resall : ", resaall)

	targetLev := strconv.FormatUint(uint64(cache.GetLLC()), 10)
	av, err := cache.GetAvailableCacheSchemata(resaall, []string{pqos.InfraGoupCOS, pqos.OSGroupCOS}, er.Type, "L"+targetLev)

	if err != nil {
		return rmderror.AppErrorf(http.StatusInternalServerError,
			"Unable to read cache schemata; %s", err.Error())
	}

	reserved := cache.GetReservedInfo()
	changedRes := make(map[string]*resctrl.ResAssociation, 0)
	candidate := make(map[string]*libutil.Bitmap, 0)

	// cache alocation settings begin (only if enabled in workload request)
	for k, v := range av {
		socketID, _ := strconv.Atoi(k)
		if !inCacheList(uint32(socketID), er.SocketIDs) && er.Type != cache.Shared {
			candidate[k], _ = libutil.NewBitmap(
				cache.GetCosInfo().CbmMaskLen,
				cache.GetCosInfo().CbmMask)
			continue
		}
		switch er.Type {
		case cache.Guarantee:
			// TODO
			// candidate[k] = v.GetBestMatchConnectiveBits(er.MaxWays, 0, true)
			candidate[k] = v.GetConnectiveBits(er.MaxWays, 0, false)
			// log.Printf("getbits",candidate[k])
		case cache.Besteffort:
			// Always to try to allocate max cache ways, if fail try to
			// get the most available ones

			freeBitmaps := v.ToBinStrings()
			var maxWays uint32
			maxWays = 0
			for _, val := range freeBitmaps {
				if val[0] == '1' {
					valLen := len(val)
					if (valLen/int(er.MinWays) > 0) && maxWays < uint32(valLen) {
						maxWays = uint32(valLen)
					}
				}
			}
			if maxWays <= 0 {
				if !reserved[cache.Besteffort].Shrink {
					return rmderror.AppErrorf(http.StatusBadRequest,
						"Not enough cache left on cache_id %s", k)
				}
				// Try to Shrink workload in besteffort pool
				cand, changed, err := shrinkBEPool(resaall, reserved[cache.Besteffort].Schemata[k], socketID, er.MinWays)
				if err != nil {
					return rmderror.AppErrorf(http.StatusInternalServerError,
						"Errors while try to shrink cache ways on cache_id %s", k)
				}
				log.Printf("Shriking cache ways in besteffort pool, candidate schemata for cache id  %d is %s", socketID, cand.ToString())
				candidate[k] = cand
				// Merge changed association to a map, we will commit this map
				// later
				for k, v := range changed {
					if _, ok := changedRes[k]; !ok {
						changedRes[k] = v
					}
				}
			} else {
				if maxWays > er.MaxWays {
					maxWays = er.MaxWays
				}
				candidate[k] = v.GetConnectiveBits(maxWays, 0, false)
			}

		case cache.Shared:
			candidate[k] = reserved[cache.Shared].Schemata[k]
		}

		if candidate[k].IsEmpty() {
			return rmderror.AppErrorf(http.StatusBadRequest,
				"Not enough cache left on cache_id %s", k)
		}
	}
	// populating cache params in rdtenforce structure with necessar values
	rdtenforce.Resall = resaall
	rdtenforce.TargetLev = targetLev
	rdtenforce.CandidateCache = candidate
	rdtenforce.ChangedRes = changedRes
	rdtenforce.Reserved = reserved
	rdtenforce.AvailableSchemata = av

	return nil
}

// This function populates the rdtenforce structure with necessary MBA params
func enforceMba(w *wltypes.RDTWorkLoad, er *wltypes.EnforceRequest, rdtenforce *wltypes.RDTEnforce) error {
	var availableSchemata map[string]*libutil.Bitmap
	var err error
	// If cache params are received as part of the request reuse the calculation in rdtenforce
	// If not then calculate
	if er.UseCache {
		availableSchemata = rdtenforce.AvailableSchemata
	} else {
		resaall := proxyclient.GetResAssociation(pqos.GetAvailableCLOSes())
		targetLev := strconv.FormatUint(uint64(cache.GetLLC()), 10)
		availableSchemata, err = cache.GetAvailableCacheSchemata(resaall, []string{pqos.InfraGoupCOS, pqos.OSGroupCOS}, "none", "L"+targetLev)
		if err != nil {
			return rmderror.AppErrorf(http.StatusInternalServerError,
				"Unable to read cache schemata; %s", err.Error())
		}
	}
	rdtenforce.CandidateMba = make(map[string]*uint32, len(availableSchemata))
	rdtenforce.TargetMba = "MB"
	defaultMBAValue := mbaMaxValue
	for k := range availableSchemata {
		socketID, ok := strconv.Atoi(k)
		if ok != nil {
			return ok
		}
		// Check the socket to which the MBA params need to be modified
		if inCacheList(uint32(socketID), er.SocketIDs) {
			rdtenforce.CandidateMba[k] = &mbaValue
		} else {
			rdtenforce.CandidateMba[k] = &defaultMBAValue
		}
	}
	return nil
}

func enforceRDT(w *wltypes.RDTWorkLoad, er *wltypes.EnforceRequest, rdtenforce *wltypes.RDTEnforce) error {
	var resAss *resctrl.ResAssociation
	var grpName string
	var err error
	// Read all the rdtenforce cache and MBA params
	targetLev := rdtenforce.TargetLev
	targetMba := rdtenforce.TargetMba
	resaall := rdtenforce.Resall
	candidateCache := rdtenforce.CandidateCache
	candidateMba := rdtenforce.CandidateMba
	changedRes := rdtenforce.ChangedRes

	shouldReturnCLOS := false
	if er.Type == cache.Shared {
		grpName, err = pqos.GetSharedCLOS()
	} else {
		grpName, err = pqos.UseAvailableCLOS()
		shouldReturnCLOS = true
	}

	if err != nil {
		log.Debugf("Failed to reserve CLOS: %v", err.Error())
		return err
	}

	// Check if COS should be returned to available pool when exiting this function.
	// Number of CLOSes is strictly limited so it's better to not waste it on non-RDT workloads
	// Skip removal if it's shared group
	defer func() {
		if shouldReturnCLOS {
			log.Warn("Releasing unused COS due to error")
			pqos.ReturnClos(w.CosName)
			w.CosName = ""
		} else {
			log.Debug("Exiting Enforce with success")
		}
	}()

	// If cache is used
	if er.UseCache {
		if er.Type == cache.Shared {
			if res, ok := resaall[grpName]; !ok {
				resAss = newResAss(candidateCache, targetLev)
			} else {
				resAss = res
			}
		} else {
			resAss = newResAss(candidateCache, targetLev)
		}
	}
	// If Mba is used
	if er.UseMba {
		// shared cache group is not allowed when MBA in use
		if er.Type == cache.Shared {
			return errors.New("MBA forbidden for shared group")
		}
		resAss = newResAssForMba(resAss, candidateMba, targetMba)
	}
	// cache allocation settings end

	if len(w.CoreIDs) >= 0 {
		bm, _ := cache.BitmapsCPUWrapper(w.CoreIDs)
		oldbm, _ := cache.BitmapsCPUWrapper(resAss.CPUs)
		bm = bm.Or(oldbm)
		resAss.CPUs = bm.ToString()
	} else {
		if len(resAss.CPUs) == 0 {
			resAss.CPUs = ""
		}
	}
	resAss.Tasks = append(resAss.Tasks, w.TaskIDs...)
	if err = proxyclient.Commit(resAss, grpName); err != nil {
		log.Errorf("Error while try to commit resource group for workload %s, group name %s", w.ID, grpName)
		return rmderror.NewAppError(http.StatusInternalServerError,
			"Error to commit resource group for workload.", err)
	}

	// loop to change shrunk resource
	// TODO: there's corners if there are multiple changed resource groups,
	// but we failed to commit one of them (worst case is the last group),
	// there's no rollback.
	// possible fix is to adding this into a task flow
	for name, res := range changedRes {
		log.Debugf("Shink %s group", name)
		if err = proxyclient.Commit(res, name); err != nil {
			log.Errorf("Error while try to commit shrunk resource group, name: %s", name)
			proxyclient.DestroyResAssociation(grpName)
			return rmderror.NewAppError(http.StatusInternalServerError,
				"Error to shrink resource group", err)
		}
	}

	// reset os group
	if err = cache.SetOSGroup(); err != nil {
		log.Errorf("Error while try to commit resource group for default group")
		proxyclient.DestroyResAssociation(grpName)
		return rmderror.NewAppError(http.StatusInternalServerError,
			"Error while try to commit resource group for default group.", err)
	}

	log.Debug("Setting cos_name to: ", grpName)
	// no errors till now - remove CLOS returning (releasing) flag
	shouldReturnCLOS = false
	w.CosName = grpName
	return nil
}

// Enforce a user request workload based on defined policy
func Enforce(w *wltypes.RDTWorkLoad) error {
	w.Status = wltypes.Failed

	l.Lock()
	defer l.Unlock()

	er := &wltypes.EnforceRequest{}
	rdtenforce := &wltypes.RDTEnforce{}
	if err := populateEnforceRequest(er, w); err != nil {
		return err
	}
	// Use cache when params received as part of request
	if er.UseCache {
		if err := enforceCache(w, er, rdtenforce); err != nil {
			return err
		}
	}
	// Use Mba when params received as part of request
	if er.UseMba {
		if err := enforceMba(w, er, rdtenforce); err != nil {
			return err
		}
	}
	// Enforce the Cache and MBA params into the resctrl
	if er.UseMba || er.UseCache {
		if err := enforceRDT(w, er, rdtenforce); err != nil {
			return err
		}
	}

	for module, params := range w.Plugins {
		log.Debugf("Sending enforce request to %v module with %v params", module, params)
		paramsMap, err := util.UnifyMapParamsTypes(params)
		if err != nil {
			return err
		}

		// params already validated in previous step so no error expected here
		valInts, _ := prepareCoreIDs(w.CoreIDs)
		paramsMap["CPUS"] = valInts
		valInts, _ = prepareCoreIDs(w.TaskIDs)
		paramsMap["TASKS"] = valInts

		result, err := proxyclient.Enforce(module, paramsMap)
		if err != nil {
			return err
		}

		// initialize before use if Plugins map doesn't exist
		if w.BackendPluginInfo == nil {
			w.BackendPluginInfo = make(map[string]string)
		}

		w.BackendPluginInfo[module] = result
	}

	w.Status = wltypes.Successful
	return nil
}

// Release Cos of the workload
func Release(w *wltypes.RDTWorkLoad) error {
	l.Lock()
	defer l.Unlock()

	for module, params := range w.Plugins {
		log.Debugf("Sending release request to %v module with %v params", module, params) // temporary log

		paramsMap, err := util.UnifyMapParamsTypes(params)
		if err != nil {
			return err
		}

		if w.BackendPluginInfo != nil {
			paramsMap["ENFORCEID"] = w.BackendPluginInfo[module]
		}

		// add core ids and process ids to params
		valInts, err := prepareCoreIDs(w.CoreIDs)
		if err != nil {
			return errors.New("Invalid params (core ids) received")
		}
		paramsMap["CPUS"] = valInts
		valInts, err = prepareCoreIDs(w.TaskIDs)
		if err != nil {
			return errors.New("Invalid params (task ids) received")
		}
		paramsMap["TASKS"] = valInts

		err = proxyclient.Release(module, paramsMap)
		if err != nil {
			return err
		}
	}

	// CosName is used only for RDT based workloads so now check it en exit if not found
	if w.CosName == "" {
		return nil
	}

	// remove workload tasks from resource group
	if len(w.TaskIDs) > 0 {
		if err := proxyclient.RemoveTasks(w.TaskIDs); err != nil {
			log.Printf("Ignore Error while remove tasks %s", err)
			return nil
		}
	}

	// remove workload cores from resource group
	if len(w.CoreIDs) > 0 {
		if err := proxyclient.RemoveCores(w.CoreIDs); err != nil {
			log.Printf("Ignore Error while remove tasks %s", err)
			return nil
		}
	}

	// Additional check needed to properly handle CLOS name for shared group
	if pqos.IsSharedCLOS(w.CosName) {
		// TODO consider simplification in future
		// (it is needed to check if any other shared workload left, maybe some internal structure will be OK)
		resaall := proxyclient.GetResAssociation(pqos.GetAvailableCLOSes())
		r, ok := resaall[w.CosName]
		if !ok {
			log.Warningf("Something is wrong with shared CLOS %s - removing from used list", w.CosName)
			pqos.ReturnClos(w.CosName)
			return nil
		}

		r.Tasks = util.SubtractStringSlice(r.Tasks, w.TaskIDs)
		cpubm, _ := cache.BitmapsCPUWrapper(r.CPUs)

		if len(w.CoreIDs) > 0 {
			wcpubm, _ := cache.BitmapsCPUWrapper(w.CoreIDs)
			cpubm = cpubm.Axor(wcpubm)
		}

		// if shared CLOS still not empty then return without releasing CLOS
		if len(r.Tasks) > 0 || !cpubm.IsEmpty() {
			log.Printf("Shared CLOS %s not empty", w.CosName)
			return cache.SetOSGroup()
		}
	}

	// set CLOS cache/MBA to default values
	if err := proxyclient.ResetCOSParamsToDefaults(w.CosName); err != nil {
		log.Errorf("%v", err)
		return nil
	}

	// return CLOSes pool (as it's non shared or shared empty)
	pqos.ReturnClos(w.CosName)

	// at the end update OS group (that is COS0 / "." in resctrl FS) accordingly
	return cache.SetOSGroup()
}

// Update a workload
func update(w, patched *wltypes.RDTWorkLoad) error {
	// if we change policy/max_cache/min_cache, release current resource group
	// and re-enforce it.
	reEnforce := false
	log.Debugf("Original WL: %v", w)
	log.Debugf("Patched WL: %v", patched)

	// check if params shall be forced by policy or one-by-one
	if len(patched.Policy) == 0 {
		// if patched workload does not define policy but original workload does
		// it's necessary to fetch all policy params and copy them to workload
		// as new configuration may not overwrite all params
		if len(w.Policy) > 0 {
			fillWorkloadByPolicy(w)
		}

		if patched.Rdt.Cache.Max != nil {
			// param manually defined - drop policy information
			w.Policy = ""
			if w.Rdt.Cache.Max == nil {
				w.Rdt.Cache.Max = patched.Rdt.Cache.Max
				reEnforce = true
			}
			if w.Rdt.Cache.Max != nil && *w.Rdt.Cache.Max != *patched.Rdt.Cache.Max {
				*w.Rdt.Cache.Max = *patched.Rdt.Cache.Max
				reEnforce = true
			}
		}

		if patched.Rdt.Cache.Min != nil {
			// param manually defined - drop policy information
			w.Policy = ""
			if w.Rdt.Cache.Min == nil {
				w.Rdt.Cache.Min = patched.Rdt.Cache.Min
				reEnforce = true
			}
			if w.Rdt.Cache.Min != nil && *w.Rdt.Cache.Min != *patched.Rdt.Cache.Min {
				*w.Rdt.Cache.Min = *patched.Rdt.Cache.Min
				reEnforce = true
			}
		}

		if isMbaSupported {
			if isMbaMbpsAvailable {
				if patched.Rdt.Mba.Percentage != nil {
					return rmderror.NewAppError(http.StatusBadRequest, "Please provide MBA in Mbps")
				}
			} else {
				if patched.Rdt.Mba.Mbps != nil {
					return rmderror.NewAppError(http.StatusBadRequest, "Please provide MBA in Percentage")
				}
			}
		}

		if patched.Rdt.Mba.Percentage != nil {
			if *patched.Rdt.Mba.Percentage > 0 && *patched.Rdt.Mba.Percentage <= cache.MaxMBAPercentage {
				w.Policy = ""
				if w.Rdt.Mba.Percentage == nil {
					w.Rdt.Mba.Percentage = patched.Rdt.Mba.Percentage
					mbaValue = *patched.Rdt.Mba.Percentage
					reEnforce = true
				}
				if w.Rdt.Mba.Percentage != nil && *w.Rdt.Mba.Percentage != *patched.Rdt.Mba.Percentage {
					*w.Rdt.Mba.Percentage = *patched.Rdt.Mba.Percentage
					mbaValue = *patched.Rdt.Mba.Percentage
					reEnforce = true
				}
			} else {
				return rmderror.NewAppError(http.StatusBadRequest, "MBA values range only from 1 to 100")
			}
		}

		if patched.Rdt.Mba.Mbps != nil {
			if *patched.Rdt.Mba.Mbps > 0 && *patched.Rdt.Mba.Mbps <= cache.MaxMBAMbps {
				w.Policy = ""
				if w.Rdt.Mba.Mbps == nil {
					w.Rdt.Mba.Mbps = patched.Rdt.Mba.Mbps
					mbaValue = *patched.Rdt.Mba.Mbps
					reEnforce = true
				}
				if w.Rdt.Mba.Mbps != nil && *w.Rdt.Mba.Mbps != *patched.Rdt.Mba.Mbps {
					*w.Rdt.Mba.Mbps = *patched.Rdt.Mba.Mbps
					mbaValue = *patched.Rdt.Mba.Mbps
					reEnforce = true
				}
			} else {
				return rmderror.NewAppError(http.StatusBadRequest, "MBA values range only from 1 to 4294967290")
			}
		}

		for module, params := range patched.Plugins {
			log.Debugf("Validating params for %v module", module) // temporary log
			if module == "cache" {
				// temporary cache is built-in module so ignore it here
				continue
			}

			// first check if params for module changed
			if reflect.DeepEqual(w.Plugins[module], params) {
				// both maps (sets of params) have same contents so skip this iteration
				continue
			}

			paramsMap, err := util.UnifyMapParamsTypes(params)
			if err != nil {
				return err

			}

			// add core ids and process ids to params
			valInts, err := prepareCoreIDs(w.CoreIDs)
			if err != nil {
				return errors.New("Invalid params (core ids) received")
			}
			paramsMap["CPUS"] = valInts

			valInts, err = prepareCoreIDs(w.TaskIDs)
			if err != nil {
				return errors.New("Invalid params (task ids) received")
			}
			paramsMap["TASKS"] = valInts

			// if params changed fetch module (if exists)
			reEnforce = true
			pluginIface, ok := plugins.Interfaces[module]
			if !ok {
				// module not loaded but requested
				return rmderror.NewAppError(http.StatusBadRequest, "Trying to use module that is not loaded")
			}
			if pluginIface == nil {
				// module seems to be loaded but interface is nil
				return rmderror.NewAppError(http.StatusInternalServerError, "Error when processing loaded modules")
			}

			// and validate params
			err = pluginIface.Validate(paramsMap)
			if err != nil {
				return rmderror.NewAppError(http.StatusBadRequest, "Invalid params received", err)
			}
		}
	} else {
		// policy defined (so should be taken as it's the priority param)
		if patched.Policy != w.Policy {
			// only if policy changed there's a need to update/reenforce workload
			w.Policy = patched.Policy
			fillWorkloadByPolicy(w)
			reEnforce = true
		}
	}

	if reEnforce == true {
		if err := Release(w); err != nil {
			return rmderror.NewAppError(http.StatusInternalServerError, "Failed to release workload",
				fmt.Errorf(""))
		}

		if len(patched.TaskIDs) > 0 {
			w.TaskIDs = patched.TaskIDs
		}
		if len(patched.CoreIDs) > 0 {
			w.CoreIDs = patched.CoreIDs
		}

		w.Plugins = patched.Plugins

		return Enforce(w)
	}

	l.Lock()
	defer l.Unlock()
	resaall := proxyclient.GetResAssociation(pqos.GetAvailableCLOSes())

	if !reflect.DeepEqual(patched.CoreIDs, w.CoreIDs) ||
		!reflect.DeepEqual(patched.TaskIDs, w.TaskIDs) {
		err := Validate(patched)
		if err != nil {
			return rmderror.NewAppError(http.StatusBadRequest, "Failed to validate workload", err)
		}

		targetResAss, ok := resaall[w.CosName]
		if !ok {
			return rmderror.NewAppError(http.StatusInternalServerError, "Can not find resource group name",
				fmt.Errorf(""))
		}

		if len(patched.TaskIDs) > 0 {
			// FIXME  Is this a bug? Seems the targetResAss.Tasks is inconsistent with w.TaskIDs
			targetResAss.Tasks = append(targetResAss.Tasks, patched.TaskIDs...)
			w.TaskIDs = patched.TaskIDs
		}
		if len(patched.CoreIDs) > 0 {
			bm, err := cache.BitmapsCPUWrapper(patched.CoreIDs)
			if err != nil {
				return rmderror.NewAppError(http.StatusBadRequest,
					"Failed to parse workload coreIDs.", err)
			}
			// TODO: check if this new CoreIDs overwrite other resource group
			targetResAss.CPUs = bm.ToString()
			w.CoreIDs = patched.CoreIDs
		}
		// commit changes
		if err = proxyclient.Commit(targetResAss, w.CosName); err != nil {
			log.Errorf("Error while try to commit resource group for workload %s, group name %s", w.ID, w.CosName)
			return rmderror.NewAppError(http.StatusInternalServerError,
				"Error to commit resource group for workload.", err)
		}
	}
	return nil
}

func getSocketIDs(taskids []string, cpubitmap string, cacheinfos *cache.Infos, cpunum int) []uint32 {
	var SocketIDs []uint32
	cpubm, _ := libutil.NewBitmap(cpunum, cpubitmap)

	for _, t := range taskids {
		af, err := proc.GetCPUAffinity(t)
		if err != nil {
			log.Warningf("Failed to get cpu affinity for task %s", t)
			// FIXME get default affinity instead of a hard code 400 cpus
			af, _ = libutil.NewBitmap(cpunum, strings.Repeat("f", 100))
		}
		cpubm = cpubm.Or(af)
	}

	// No warry, cpubitmap is empty if taskids is None
	for _, c := range cacheinfos.Caches {
		// Okay, NewBitmap only support string list if we using human style
		bm, _ := libutil.NewBitmap(cpunum, strings.Split(c.ShareCPUList, "\n"))
		if !cpubm.And(bm).IsEmpty() {
			SocketIDs = append(SocketIDs, c.ID)
		}
	}
	return SocketIDs
}

func inCacheList(socket uint32, socketList []uint32) bool {
	// TODO: if this case, workload has taskids.
	// Later we need to have abilitity to discover if has taskset
	// to pin this taskids on a cpuset or not, for now we allocate
	// cache on all cache.
	// FIXME: this shouldn't happen here actually
	if len(socketList) == 0 {
		return true
	}

	for _, c := range socketList {
		if socket == c {
			return true
		}
	}
	return false
}

func populateEnforceRequest(req *wltypes.EnforceRequest, w *wltypes.RDTWorkLoad) error {
	w.Status = wltypes.None
	cpubitstr := ""
	if len(w.CoreIDs) >= 0 {
		bm, err := cache.BitmapsCPUWrapper(w.CoreIDs)
		if err != nil {
			return rmderror.NewAppError(http.StatusBadRequest,
				"Failed to Parse workload coreIDs.", err)
		}
		cpubitstr = bm.ToString()
	}

	cacheinfo := &cache.Infos{}
	cacheinfo.GetByLevel(cache.GetLLC())

	cpunum := cpu.HostCPUNum()
	if cpunum == 0 {
		return rmderror.AppErrorf(http.StatusInternalServerError,
			"Unable to get Total CPU numbers on Host")
	}

	req.SocketIDs = getSocketIDs(w.TaskIDs, cpubitstr, cacheinfo, cpunum)

	// if policy not defined in workload then use values from manually defined params
	// (assuming RDTWorkLoad object has been validated before and only some safe-checks needed)
	if len(w.Policy) == 0 {
		if w.Rdt.Cache.Min != nil {
			req.MinWays = *w.Rdt.Cache.Min
		}
		if w.Rdt.Cache.Max != nil {
			req.MaxWays = *w.Rdt.Cache.Max
		}
		if w.Rdt.Cache.Min != nil && w.Rdt.Cache.Max != nil {
			req.UseCache = true
		}
		// Check if MBA is available and enabled in the host
		// MBA to be used only for Guaranteed Cache Request
		if w.Rdt.Mba.Percentage != nil || w.Rdt.Mba.Mbps != nil {
			if !isMbaSupported {
				req.UseMba = false
				log.Error("Mba is not supported in this machine")
				return rmderror.NewAppError(http.StatusInternalServerError,
					"MBA is not supported in this machine")
			}
			if flag, _ := proc.IsEnableMba(); !flag {
				req.UseMba = false
				log.Error("Mba is not enabled. Enable Mba")
				return rmderror.NewAppError(http.StatusInternalServerError,
					"Please enable MBA in resctrl fs")
			}
			if (w.Rdt.Cache.Min == nil && w.Rdt.Cache.Max == nil) ||
				(req.UseCache && (*w.Rdt.Cache.Max == *w.Rdt.Cache.Min && *w.Rdt.Cache.Max > 0 ||
					*w.Rdt.Cache.Max != *w.Rdt.Cache.Min && mbaValue == mbaMaxValue ||
					*w.Rdt.Cache.Max == 0 && *w.Rdt.Cache.Min == 0 && mbaValue == mbaMaxValue)) {
				req.UseMba = true
			} else {
				req.UseMba = false
				log.Error("Mba can be used only guaranteed Cache Request")
				return rmderror.NewAppError(http.StatusInternalServerError,
					"MBA is only supported for Guarantee Cache Request")
			}
		}
	} else {
		policy, err := policy.GetDefaultPolicy(w.Policy)
		if err != nil {
			return rmderror.NewAppError(http.StatusInternalServerError,
				"Could not find the Policy.", err)
		}

		maxWays, okMax := policy["cache"]["max"].(int64)
		if !okMax {
			log.Error("Max cache reading error - cache way assignment will be skipped")
		} else {
			req.MaxWays = uint32(maxWays)
		}

		minWays, okMin := policy["cache"]["min"].(int64)
		if !okMin {
			log.Error("Min cache reading error - cache way assignment will be skipped")
		} else {
			req.MinWays = uint32(minWays)
		}

		// use cache params only if both defined
		if okMax && okMin {
			req.UseCache = true
		}
	}

	if req.MinWays > req.MaxWays {
		return rmderror.NewAppError(http.StatusInternalServerError,
			"Min cache value cannot be greater than max cache value")
	}

	if req.UseCache {
		var err error
		req.Type, err = cache.GetCachePoolName(req.MaxWays, req.MinWays)
		if err != nil {
			return rmderror.NewAppError(http.StatusBadRequest,
				"Bad cache ways request",
				err)
		}
	}

	return nil
}

func newResAss(r map[string]*libutil.Bitmap, level string) *resctrl.ResAssociation {
	newResAss := resctrl.ResAssociation{}
	newResAss.CacheSchemata = make(map[string][]resctrl.CacheCos)

	targetLev := "L" + level

	// fmt.Println("newResAss : ", r)

	for k, v := range r {
		cacheID, _ := strconv.Atoi(k)
		newcos := resctrl.CacheCos{ID: uint8(cacheID), Mask: v.ToString()}
		newResAss.CacheSchemata[targetLev] = append(newResAss.CacheSchemata[targetLev], newcos)

		log.Debugf("Newly created Mask for Cache %s is %s", k, newcos.Mask)
	}
	return &newResAss
}

func newResAssForMba(resAss *resctrl.ResAssociation, candidate map[string]*uint32, targetMba string) *resctrl.ResAssociation {
	if resAss == nil {
		resAss = &resctrl.ResAssociation{}
	}
	resAss.MbaSchemata = make(map[string][]resctrl.MbaCos)
	for k, v := range candidate {
		MbaID, _ := strconv.Atoi(k)
		newcos := resctrl.MbaCos{ID: uint8(MbaID), Mba: *v}
		resAss.MbaSchemata[targetMba] = append(resAss.MbaSchemata[targetMba], newcos)
	}
	return resAss
}

// shrinkBEPool requres to provide cacheid of the request, MinCache ways (
// because we lack cache now if we need to shrink), of cause resassociations
// besteffort pool reserved cache way bitmap.
// returns: bitmap we allocated for the new request
// returns: a map[string]*resctrl.ResAssociation as we changed other workloads'
// cache ways, need to reflect them into resctrl fs.
// returns: error if internal error happens.
func shrinkBEPool(resaall map[string]*resctrl.ResAssociation,
	reservedSchemata *libutil.Bitmap,
	cacheID int,
	reqways uint32) (*libutil.Bitmap, map[string]*resctrl.ResAssociation, error) {

	besteffortRes := make(map[string]*resctrl.ResAssociation)
	dbc, _ := db.NewDB()
	// do a copy
	availableSchemata := &(*reservedSchemata)
	targetLev := strconv.FormatUint(uint64(cache.GetLLC()), 10)
	for name, v := range resaall {
		if strings.HasSuffix(name, "-"+cache.Besteffort) {
			besteffortRes[name] = v
			ws, _ := dbc.QueryWorkload(map[string]interface{}{
				"CosName": name})
			if len(ws) == 0 {
				return nil, besteffortRes, fmt.Errorf(
					"Internal error, can not find exsting workload for resource group name %s", name)
			}
			cosSchemata, _ := cache.BitmapsCacheWrapper(v.CacheSchemata["L"+targetLev][cacheID].Mask)
			// TODO: need find a better way to reduce the cache way fragments
			// as currently we are using map to keep resctrl group, it's non-order
			// so it's little hard to get which resctrl group next to which.
			// just using max - min slot to shrink the cache. Hence, the result
			// would only shrink one of the resource group to min one
			minSchemata := cosSchemata.GetConnectiveBits(*ws[0].Rdt.Cache.Min, 0, false)
			availableSchemata = availableSchemata.Axor(minSchemata)
		}
	}
	// I would like to allocate cache from low to high, this will help to
	// reduce cos
	candidateSchemata := availableSchemata.GetConnectiveBits(reqways, 0, true)

	// loop besteffortRes to find which association need to be changed.
	changedRes := make(map[string]*resctrl.ResAssociation)
	for name, v := range besteffortRes {
		cosSchemata, _ := cache.BitmapsCacheWrapper(v.CacheSchemata["L"+targetLev][cacheID].Mask)
		tmpSchemataStr := cosSchemata.Axor(candidateSchemata).ToString()
		if tmpSchemataStr != cosSchemata.ToString() {
			// Changing pointers, the change will be reflact to the origin one
			v.CacheSchemata["L"+targetLev][cacheID].Mask = tmpSchemataStr
			changedRes[name] = v
		}
	}

	return candidateSchemata, changedRes, nil
}

//GetByUUID function gets workload from database by UUID (OpenStack instance identifier)
func GetByUUID(uuid string) (result wltypes.RDTWorkLoad, err error) {
	if workloadDatabase == nil {
		return result, rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}
	result, err = workloadDatabase.GetWorkloadByUUID(uuid)
	if err != nil {
		e := rmderror.NewAppError(rmderror.NotFound, "Failed to get workload by UUID from database", err)
		return result, e
	}
	return result, nil
}

//Delete function deletes workload from data base
func Delete(wl *wltypes.RDTWorkLoad) error {
	if workloadDatabase == nil {
		return rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}
	err := workloadDatabase.DeleteWorkload(wl)
	if err != nil {
		return rmderror.NewAppError(rmderror.InternalServer, "Failed to remove workload from database", err)
	}
	return nil
}

//Create function creates workload in data base
func Create(wl *wltypes.RDTWorkLoad) error {
	if workloadDatabase == nil {
		return rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}
	err := workloadDatabase.CreateWorkload(wl)
	if err != nil {
		return rmderror.NewAppError(rmderror.InternalServer, "Failed to create workload in database", err)
	}
	return nil
}

//GetAll gets list of workloads
func GetAll() ([]wltypes.RDTWorkLoad, error) {
	ws := []wltypes.RDTWorkLoad{}
	if workloadDatabase == nil {
		return ws, rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}
	ws, err := workloadDatabase.GetAllWorkload()
	if err != nil {
		return ws, rmderror.NewAppError(http.StatusInternalServerError, err.Error())
	}
	return ws, nil
}

//GetWorkloadByID function gets workload from data base by ID
func GetWorkloadByID(id string) (result wltypes.RDTWorkLoad, err error) {
	if workloadDatabase == nil {
		return result, rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}

	result, err = workloadDatabase.GetWorkloadByID(id)
	if err != nil {
		e := rmderror.NewAppError(rmderror.NotFound, "Failed to get workload by ID from database", err)
		return result, e
	}
	return result, nil
}

//validateInDB validates the request workload object in db
func validateInDB(wl *wltypes.RDTWorkLoad) error {
	if workloadDatabase == nil {
		return rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}

	if err := workloadDatabase.ValidateWorkload(wl); err != nil {
		return rmderror.NewAppError(rmderror.InternalServer, "Workload validation in database failed", err)
	}
	return nil
}

func updateInDB(w *wltypes.RDTWorkLoad) error {
	if workloadDatabase == nil {
		return rmderror.NewAppError(http.StatusInternalServerError, "Service database not initialized")
	}
	if err := workloadDatabase.UpdateWorkload(w); err != nil {
		return rmderror.NewAppError(rmderror.InternalServer, "Failed to update workload in database", err)
	}

	return nil
}

// Validate the request workload object is validated.
func Validate(w *wltypes.RDTWorkLoad) error {
	err := validate(w)
	if err != nil {
		log.Errorf("Failed to validate workload due to reason: %s", err.Error())
		return err
	}

	err = validateInDB(w)
	if err != nil {
		log.Errorf("Failed to validate workload in database due to reason: %s", err.Error())
		return err
	}
	return nil
}

// Update a workload
func Update(w, patched *wltypes.RDTWorkLoad) error {

	dbContentValidation()

	err := update(w, patched)
	if err != nil {
		log.Error("Failed to update/patch workload")
		return err
	}

	err = updateInDB(w)
	if err != nil {
		log.Error("Failed to update/patch workload in database")
		return err
	}

	return nil
}

// Init responsible for database creation
// this function should be exported to give possibility to use DB
// for example by Openstack without need of registering workload module
func Init() error {
	temp, err := db.NewDB()
	if err != nil {
		log.Error("Cannot create database")
	} else {
		workloadDatabase = temp
		go startDBContentValidation()
	}
	// CLOS pool has to be initialized before it can be used
	if err := pqos.InitCLOSPool(); err != nil {
		log.Errorf("Failed to initialize CLOS pool: %v", err.Error())
		return errors.New("CLOS pool initialization failure")
	}
	isMbaSupported, err = proc.IsMbaAvailable()
	if err != nil {
		return err
	}
	isL3CATSupported, err = proc.IsL3CatAvailable()
	if err != nil {
		return err
	}
	// Additional check for MBA mode (configured vs. used in workloads in db) needed due to 2 MBA modes and PQOS usage
	// NOTE TODO: In future it will be good to validate param of each plugin (including RDT) used in stored workloads
	// - get all stored workloads
	allWorkloads, err := workloadDatabase.GetAllWorkload()
	if err != nil {
		return fmt.Errorf("Failed to get data from DB during workload.Init(): %v", err.Error())
	}
	if len(allWorkloads) > 0 {
		// - get configured MBA mode
		rdtc := cacheconf.RDTConfig{MBAMode: "percentage"} // default value used if not set in config file
		err = viper.UnmarshalKey("rdt", &rdtc)
		if err != nil {
			return errors.New("Failed to check RDT config in rmd.toml")
		}
		var forceFlag bool
		forceFlagVar := pflag.Lookup("force-config")
		// additional safety check for unit tests (as pflag is not initialized then)
		if forceFlagVar != nil {
			forceFlagString := forceFlagVar.Value.String()
			if strings.ToLower(forceFlagString) == "true" {
				forceFlag = true
			}
		}
		// end of safety check
		var invalidWl bool
		for _, wl := range allWorkloads {
			// by default assume that workload is OK
			invalidWl = false
			if wl.Rdt.Mba.Percentage != nil && rdtc.MBAMode != "percentage" {
				// mark as invalid
				invalidWl = true
			}
			if wl.Rdt.Mba.Mbps != nil && rdtc.MBAMode != "mbps" {
				// mark as invalid
				invalidWl = true
			}
			if invalidWl {
				// if force flag set - remove workload, otherwise return with error
				if forceFlag {
					// delete workload from database (it should be already removed from platform by root process)
					log.Warningf("Workload %v contains incorrect MBA mode - removing from database", wl.ID)
					workloadDatabase.DeleteWorkload(&wl)
				} else {
					return fmt.Errorf("Workload in database contains MBA setting (percentage) not compatible with current configuration (%v)", rdtc.MBAMode)
				}
			} else {
				// workload is valid so check it's cos_name and mark it as used
				if len(wl.CosName) > 0 {
					err := pqos.MarkCLOSasUsed(wl.CosName)
					if err != nil {
						return fmt.Errorf("Problem with CLOS of workload from database: %v", err.Error())
					}
				}
			}
		}
	}

	if isMbaSupported {
		isMbaMbpsAvailable = proc.GetMbaMbpsMode()
		if isMbaMbpsAvailable {
			mbaMaxValue = cache.MaxMBAMbps
		} else {
			mbaMaxValue = cache.MaxMBAPercentage
		}
	}
	return err
}

// prepareCoreIDs is responsible for preparting coreIDs
func prepareCoreIDs(w []string) ([]int, error) {
	coreids := []int{}

	for _, value := range w {

		// code to handle cases like "12-16" which should return "12 13 14 15 16"
		dashPosition := strings.Index(value, "-")
		if dashPosition != (-1) {
			// '-' exists
			beforeDashStr := strings.TrimSpace(value[:dashPosition])
			afterDashStr := strings.TrimSpace(value[dashPosition+1:])

			beforeDash, err := strconv.Atoi(beforeDashStr)
			if err != nil {
				log.Errorf("Failed to convert coreID value %v from string to int", beforeDashStr)
				return coreids, err
			}

			afterDash, err := strconv.Atoi(afterDashStr)
			if err != nil {
				log.Errorf("Failed to convert coreID value %v from string to int", afterDashStr)
				return coreids, err
			}
			// syntax like "8-3" is wrong so need additional check here
			if beforeDash > afterDash {
				log.Errorf("Wrong syntax for coreIDs -> %s", value)
				return coreids, fmt.Errorf("Wrong syntax for coreIDs")
			}

			i := beforeDash
			for i <= afterDash {
				coreids = append(coreids, i)
				i++
			}
		} else {
			intid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				log.Errorf("Invalid core id %s - cannot continue", value)
				return coreids, fmt.Errorf("Invalid core id in array: %s", value)
			}
			coreids = append(coreids, intid)
		}
	}

	return coreids, nil
}

// shouldRemoveWorkload checks if all processes for workload exists
// return false if at least one task from workload still exists in the system
func shouldRemoveWorkload(w *wltypes.RDTWorkLoad) bool {

	for _, task := range w.TaskIDs {

		result, err := os.Stat("/proc/" + task)
		if (err == nil) && (result.IsDir() == true) {
			return false
		}
	}
	//workload should be removed
	return true
}

func startDBContentValidation() {
	appconf := appConf.NewConfig()
	interval := appconf.Def.DbValidatorInterval
	if interval <= 0 {
		log.Errorf("Failed to start DBValidator due to wrong interval value: %v", interval)
		return
	}
	for {
		dbContentValidation()
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// dbContentValidation checks DB for outdated workloads
// when related taskIDs/processIDs don't exist anymore
func dbContentValidation() {

	l.Lock()
	defer l.Unlock()
	ws, err := GetAll()
	if err == nil {
		for _, singleWorkload := range ws {
			// validation only for workloads/policies related with taskID/processID
			if len(singleWorkload.TaskIDs) != 0 {
				// remove from DB workloads which are related with not existing
				// any more tasks/processes in the system (remove when all tasks doesn't exist)
				if shouldRemoveWorkload(&singleWorkload) {
					err := Delete(&singleWorkload)
					if err != nil {
						// just log here
						log.Errorf("dbContentValidation failed to delete invalid workload from db: %s", err)
					}
					// inform about deleted workloads
					log.Infof("Workload %v deleted by DBValidator", singleWorkload)
				}
			}
		}
	} else {
		log.Errorf("dbContentValidation failed to validate DB for outdated workloads")
	}
}
