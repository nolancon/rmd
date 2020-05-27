package workload

// workload api objects to represent resources in RMD

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"

	proxyclient "github.com/intel/rmd/internal/proxy/client"
	"github.com/intel/rmd/modules/cache"
	"github.com/intel/rmd/utils/cpu"
	"github.com/intel/rmd/utils/resctrl"

	libutil "github.com/intel/rmd/utils/bitmap"
	"github.com/intel/rmd/utils/proc"

	"github.com/intel/rmd/internal/db"
	rmderror "github.com/intel/rmd/internal/error"
	"github.com/intel/rmd/modules/policy"
	"github.com/intel/rmd/modules/pstate"
	wltypes "github.com/intel/rmd/modules/workload/types"
	util "github.com/intel/rmd/utils"
)

const (
	defaultMBAPercentage uint32 = 100
)

var l sync.Mutex

// database for storing all active workloads
var workloadDatabase db.DB

// Flag to check if MBA and L3 CAT is supported
var isMbaSupported, isL3CATSupported bool

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
	maxWays, err := strconv.Atoi(policy["cache"]["max"])
	if err == nil {
		wrkld.Rdt.Cache.Max = new(uint32)
		*wrkld.Rdt.Cache.Max = uint32(maxWays)
	}

	minWays, err := strconv.Atoi(policy["cache"]["min"])
	if err == nil {
		wrkld.Rdt.Cache.Min = new(uint32)
		*wrkld.Rdt.Cache.Min = uint32(minWays)
	}

	if (wrkld.Rdt.Cache.Min != nil && wrkld.Rdt.Cache.Max == nil) || (wrkld.Rdt.Cache.Min == nil && wrkld.Rdt.Cache.Max != nil) {
		return fmt.Errorf("Invalid policy - exactly one *Cache param defined")
	}

	// check if Power/P-State module is enabled
	if pstate.Instance != nil {
		log.Debugf("Get pstate params for %s policy and fill workload", wrkld.Policy)
		if ratioString, ok := policy["pstate"]["ratio"]; ok {
			// convert string to float64
			ratio, err := strconv.ParseFloat(ratioString, 64)
			if err != nil {
				return rmderror.NewAppError(http.StatusInternalServerError,
					"Broken policy for P-State. %v", err)
			}
			wrkld.PState.Ratio = new(float64)
			*wrkld.PState.Ratio = ratio
			wrkld.PState.Monitoring = new(string)
			*wrkld.PState.Monitoring = "on"
		} else {
			return fmt.Errorf("Error while getting Ratio from P-State policy. %v", err)
		}

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
		// there have to be both cache values or none of them
		if (w.Rdt.Cache.Max == nil && w.Rdt.Cache.Min != nil) || (w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min == nil) {
			return fmt.Errorf("Need to provide both *_cache or none of them")
		}
		// If MBA values are provided :
		// 1. Check if its a Cache guaranteed request
		// 2. Check if MBA value is range of 1 to 100
		// 3. If any of the above fails then return error
		if w.Rdt.Mba.Percentage != nil {
			if w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min != nil &&
				((*w.Rdt.Cache.Max != *w.Rdt.Cache.Min && *w.Rdt.Mba.Percentage != 100) ||
					(*w.Rdt.Cache.Min == 0 && *w.Rdt.Cache.Max == 0 && *w.Rdt.Mba.Percentage != 100)) {
				return fmt.Errorf("MBA only supported for Guaranteed Request and not for BestEffort and Shared")
			}
			if *w.Rdt.Mba.Percentage > 100 || *w.Rdt.Mba.Percentage <= 0 {
				return fmt.Errorf("MBA values in percentage range from 1 to 100")
			}
		}

		if isL3CATSupported && isMbaSupported {
			if w.Rdt.Cache.Max == nil && w.Rdt.Cache.Min == nil && w.Rdt.Mba.Percentage != nil {
				return fmt.Errorf("Need to provide both cache and mba for better performance")
			}
		} else {
			if isL3CATSupported {
				if w.Rdt.Mba.Percentage != nil {
					return fmt.Errorf("This machine supports only cache and not MBA")
				}
			} else {
				if w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min != nil {
					return fmt.Errorf("This machine supports only MBA and not cache")
				}
			}
		}

		if w.PState.Ratio != nil || w.PState.Monitoring != nil {
			// when P-State related setting forced check if P-State plugin enabled
			if pstate.Instance == nil {
				return fmt.Errorf("P-State configuration given while plugin not enabled")
			}
		}
		if w.PState.Ratio != nil && *w.PState.Ratio <= 0.0 {
			return fmt.Errorf("Invalid P-State Branch Ratio given")
		}
		if w.PState.Ratio != nil && w.PState.Monitoring == nil {
			// when Branch Ratio given then implicitly enable monitoring
			w.PState.Monitoring = new(string)
			*w.PState.Monitoring = "on"
		}
		if w.PState.Monitoring != nil {
			switch *w.PState.Monitoring {
			case "on":
				fallthrough
			case "off":
				break
			case "ON":
				*w.PState.Monitoring = "on"
				break
			case "OFF":
				*w.PState.Monitoring = "off"
				break
			default:
				return fmt.Errorf("Invalid P-State Monitoring value")
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
	// - Cache.Max && Cache.min
	// - PState.Ratio || PState.Monitoring
	if w.Rdt.Cache.Max != nil && w.Rdt.Cache.Min != nil {
		// Cache params defined
		return nil
	}
	if w.PState.Ratio != nil || w.PState.Monitoring != nil {
		// P-State params defined
		return nil
	}

	if w.Rdt.Mba.Percentage != nil {
		// MBA params defined
		return nil
	}

	// if reached this point then something went wrong
	return fmt.Errorf("No Cache/MBA/PState params in workload")
}

func enforceCache(w *wltypes.RDTWorkLoad, er *wltypes.EnforceRequest, rdtenforce *wltypes.RDTEnforce) error {

	resaall := proxyclient.GetResAssociation()

	targetLev := strconv.FormatUint(uint64(cache.GetLLC()), 10)
	av, err := cache.GetAvailableCacheSchemata(resaall, []string{"infra", "."}, er.Type, "L"+targetLev)
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
		resaall := proxyclient.GetResAssociation()
		targetLev := strconv.FormatUint(uint64(cache.GetLLC()), 10)
		availableSchemata, err = cache.GetAvailableCacheSchemata(resaall, []string{"infra", "."}, "none", "L"+targetLev)
		if err != nil {
			return rmderror.AppErrorf(http.StatusInternalServerError,
				"Unable to read cache schemata; %s", err.Error())
		}
	}
	rdtenforce.CandidateMba = make(map[string]*uint32, len(availableSchemata))
	rdtenforce.TargetMba = "MB"
	defaultMBAValue := defaultMBAPercentage
	for k := range availableSchemata {
		socketID, ok := strconv.Atoi(k)
		if ok != nil {
			return ok
		}
		// Check the socket to which the MBA params need to be modified
		if inCacheList(uint32(socketID), er.SocketIDs) {
			rdtenforce.CandidateMba[k] = w.Rdt.Mba.Percentage
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
	reserved := rdtenforce.Reserved
	targetLev := rdtenforce.TargetLev
	targetMba := rdtenforce.TargetMba
	resaall := rdtenforce.Resall
	candidateCache := rdtenforce.CandidateCache
	candidateMba := rdtenforce.CandidateMba
	changedRes := rdtenforce.ChangedRes
	// If cache is used
	if er.UseCache {
		if er.Type == cache.Shared {
			grpName = reserved[cache.Shared].Name
			if res, ok := resaall[grpName]; !ok {
				resAss = newResAss(candidateCache, targetLev)
			} else {
				resAss = res
			}
		} else {
			resAss = newResAss(candidateCache, targetLev)
			if len(w.TaskIDs) > 0 {
				grpName = strings.Join(w.TaskIDs, "_") + "-" + er.Type
			} else if len(w.CoreIDs) > 0 {
				grpName = strings.Join(w.CoreIDs, "_") + "-" + er.Type
			}
		}
	}
	// If Mba is used
	if er.UseMba {
		if er.Type == cache.Shared {
			grpName = reserved[cache.Shared].Name
		} else {
			if er.Type == cache.Besteffort {
				grpName = strings.Join(w.TaskIDs, "_") + "-" + er.Type
			} else {
				if len(w.TaskIDs) > 0 {
					grpName = strings.Join(w.TaskIDs, "_") + "-" + "guarantee"
				} else if len(w.CoreIDs) > 0 {
					grpName = strings.Join(w.CoreIDs, "_") + "-" + "guarantee"
				}
			}
		}
		resAss = newResAssForMba(resAss, candidateMba, targetMba)
	}
	// cache alocation settings end

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
	// but we failed to commit one of them (worest case is the last group),
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
	if err := enforceRDT(w, er, rdtenforce); err != nil {
		return err
	}

	// p-state settings begin
	if pstate.Instance != nil {
		// if P-State used in this Workload then enforce it
		if w.PState.Ratio != nil || w.PState.Monitoring != nil {
			log.Debugf("Enforcing P-State enebled Workload")
			// at this point we don't know exact size due to possibility of "3-8" syntax usage
			// coreids is []int type
			coreids, err := prepareCoreIDs(w.CoreIDs)
			if err != nil {
				log.Errorf("Failed to prepare core IDs list for enforce")
				return err
			}

			// prepare generic params for module
			params := make(map[string]interface{})
			if w.PState.Monitoring != nil {
				params["ratio"] = *w.PState.Ratio
			}
			if w.PState.Monitoring != nil {
				params["monitoring"] = *w.PState.Monitoring
			}

			err = pstate.Instance.Enforce(coreids, []int{}, params)
			if err != nil {
				log.Warningf("Could not patch pstate config: %s", err)
			}
		}
	}
	// p-state settings end

	w.Status = wltypes.Successful
	return nil
}

// Release Cos of the workload
func Release(w *wltypes.RDTWorkLoad) error {
	l.Lock()
	defer l.Unlock()

	resaall := proxyclient.GetResAssociation()

	r, ok := resaall[w.CosName]

	if !ok {
		log.Warningf("Could not find COS %s.", w.CosName)
		return nil
	}

	r.Tasks = util.SubtractStringSlice(r.Tasks, w.TaskIDs)
	cpubm, _ := cache.BitmapsCPUWrapper(r.CPUs)

	if len(w.CoreIDs) > 0 {
		wcpubm, _ := cache.BitmapsCPUWrapper(w.CoreIDs)
		cpubm = cpubm.Axor(wcpubm)
	}

	// check if P-State related params should be verified
	if pstate.Instance != nil {
		// if P-State used in this Workload then remove it
		if w.PState.Ratio != nil || w.PState.Monitoring != nil {
			log.Debugf("Releasing P-State enebled Workload")
			// convert core ids in []string into coreids in []int

			// coreids is []int type
			coreids, err := prepareCoreIDs(w.CoreIDs)
			if err != nil {
				log.Errorf("Failed to prepare core IDs list for release")
				return err
			}

			err = pstate.Instance.Release(coreids, []int{}, map[string]interface{}{})
			if err != nil {
				log.Warningf("Could not release pstate config(s): %s", err)
			}
		}
	}

	// safely remove resource group if no tasks and cpu bit map is empty
	if len(r.Tasks) < 1 && cpubm.IsEmpty() {
		log.Printf("Remove resource group: %s", w.CosName)
		if err := proxyclient.DestroyResAssociation(w.CosName); err != nil {
			return err
		}
		return cache.SetOSGroup()
	}
	// remove workload task ids from resource group
	if len(w.TaskIDs) > 0 {
		if err := proxyclient.RemoveTasks(w.TaskIDs); err != nil {
			log.Printf("Ignore Error while remove tasks %s", err)
			return nil
		}
	}

	if len(w.CoreIDs) > 0 {
		r.CPUs = cpubm.ToString()
		return proxyclient.Commit(r, w.CosName)
	}
	return nil
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

		if patched.Rdt.Mba.Percentage != nil {
			if *patched.Rdt.Mba.Percentage > 0 && *patched.Rdt.Mba.Percentage <= 100 {
				w.Policy = ""
				if w.Rdt.Mba.Percentage == nil {
					w.Rdt.Mba.Percentage = patched.Rdt.Mba.Percentage
					reEnforce = true
				}
				if w.Rdt.Mba.Percentage != nil && *w.Rdt.Mba.Percentage != *patched.Rdt.Mba.Percentage {
					*w.Rdt.Mba.Percentage = *patched.Rdt.Mba.Percentage
					reEnforce = true
				}
			} else {
				return rmderror.NewAppError(http.StatusBadRequest, "MBA values range only from 1 to 100")
			}
		}

		if patched.PState.Ratio != nil {
			// param manually defined - drop policy information
			w.Policy = ""
			if w.PState.Ratio == nil {
				w.PState.Ratio = new(float64)
			}
			if *w.PState.Ratio != *patched.PState.Ratio {
				*w.PState.Ratio = *patched.PState.Ratio
				reEnforce = true
			}
		}

		if patched.PState.Monitoring != nil {
			// param manually defined - drop policy information
			w.Policy = ""
			if w.PState.Monitoring == nil {
				w.PState.Monitoring = new(string)
			}
			if *w.PState.Monitoring != *patched.PState.Monitoring {
				*w.PState.Monitoring = *patched.PState.Monitoring
				reEnforce = true
			}
		}
	} else {
		// policy defined (so shoul be taken as it's the priority param)
		if patched.Policy != w.Policy {
			// only if policy changed there's a need to update/reenforce workload
			w.Policy = patched.Policy
			fillWorkloadByPolicy(w)
			reEnforce = true
		}
	}

	if reEnforce == true {
		if err := Release(w); err != nil {
			return rmderror.NewAppError(http.StatusInternalServerError, "Faild to release workload",
				fmt.Errorf(""))
		}

		if len(patched.TaskIDs) > 0 {
			w.TaskIDs = patched.TaskIDs
		}
		if len(patched.CoreIDs) > 0 {
			w.CoreIDs = patched.CoreIDs
		}
		return Enforce(w)
	}

	l.Lock()
	defer l.Unlock()
	resaall := proxyclient.GetResAssociation()

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
					"Failed to Pareser workload coreIDs.", err)
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
		if w.Rdt.Mba.Percentage != nil {
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
					*w.Rdt.Cache.Max != *w.Rdt.Cache.Min && *w.Rdt.Mba.Percentage == 100 ||
					*w.Rdt.Cache.Max == 0 && *w.Rdt.Cache.Min == 0 && *w.Rdt.Mba.Percentage == 100)) {
				req.UseMba = true
			} else {
				req.UseMba = false
				log.Error("Mba can be used only guaranteed Cache Request")
				return rmderror.NewAppError(http.StatusInternalServerError,
					"MBA is only supported for Guarantee Cache Request")
			}
		}
		if w.PState.Ratio != nil {
			req.PState = true
			req.PStateBR = *w.PState.Ratio
		}
		if w.PState.Monitoring != nil {
			// copy monitoring setting to request
			if *w.PState.Monitoring == "on" {
				req.PStateMonitoring = true
			} else {
				req.PStateMonitoring = false
			}
			// mark that PState settings used in this request
			req.PState = true
		}
	} else {
		policy, err := policy.GetDefaultPolicy(w.Policy)
		if err != nil {
			return rmderror.NewAppError(http.StatusInternalServerError,
				"Could not find the Policy.", err)
		}

		maxWays, errMax := strconv.Atoi(policy["cache"]["max"])
		if errMax == nil {
			req.MaxWays = uint32(maxWays)
		} else {
			log.Error("Max cache reading error - cache way assignment will be skipped")
		}

		minWays, errMin := strconv.Atoi(policy["cache"]["min"])
		if errMin == nil {
			req.MinWays = uint32(minWays)
		} else {
			log.Error("Min cache reading error - cache way assignment will be skipped")
		}

		// use cache params only if both defined
		if errMax == nil && errMin == nil {
			req.UseCache = true
		}

		// check if Power/P-State module is enabled
		if pstate.Instance != nil {
			log.Debugf("Get pstate params for %s policy and populate enforce request", w.Policy)
			if ratioString, ok := policy["pstate"]["ratio"]; ok {
				// convert string to float64
				ratio, err := strconv.ParseFloat(ratioString, 64)
				if err != nil {
					return rmderror.NewAppError(http.StatusInternalServerError,
						"Broken policy for P-State", err)
				}
				req.PState = true
				req.PStateBR = ratio
				req.PStateMonitoring = true
			} else {
				return rmderror.NewAppError(http.StatusInternalServerError,
					"Error while getting Ratio from P-State policy", err)
			}
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
	}
	isMbaSupported, err = proc.IsMbaAvailable()
	if err != nil {
		return err
	}
	isL3CATSupported, err = proc.IsL3CatAvailable()
	if err != nil {
		return err
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
