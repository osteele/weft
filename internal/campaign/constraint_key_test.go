package campaign

import (
	"reflect"
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

// Two groups that collide on constraintKey share one cached provider search,
// so any field the search sends server-side must change the key. A field
// missing from the key is a one-directional market-narrowing bug: whichever
// group searches first, its predicate filters the offers the other group
// sees. MinHostRAMGB (Vast.ai cpu_ram>=) shipped that way.
//
// The variants map must name every exported OfferConstraints field except
// those tagged `axis:"-"` (declared non-constraining — they cannot narrow a
// search, so the key need not split the cache on them). Adding a field to the
// struct fails this test until someone decides how the key covers it — the
// same forcing move the axis-tag contract in cloud.OfferConstraints makes for
// enforcement.
func TestConstraintKeyCoversEveryConstraintField(t *testing.T) {
	base := cloud.OfferConstraints{
		GPUClass:             "a100",
		MinGPUMemGB:          40,
		MinDiskGB:            60,
		MinReliability:       0.95,
		MinDriverVersion:     550,
		MinCUDAVersion:       "12.4",
		NumGPUs:              2,
		Interconnect:         "nvlink",
		ExcludeGeos:          []string{"CN"},
		MinCPUCoresEffective: 16,
		MinHostRAMGB:         128,
		InstanceType:         cloud.InstanceTypeOnDemand,
		RunpodCloudType:      "secure",
	}
	variants := map[string]cloud.OfferConstraints{
		"GPUClass":             {GPUClass: "h100"},
		"MinGPUMemGB":          {MinGPUMemGB: 80},
		"MinDiskGB":            {MinDiskGB: 100},
		"MinReliability":       {MinReliability: 0.5},
		"MinDriverVersion":     {MinDriverVersion: 535},
		"MinCUDAVersion":       {MinCUDAVersion: "12.8"},
		"NumGPUs":              {NumGPUs: 4},
		"Interconnect":         {Interconnect: "pcie"},
		"ExcludeGeos":          {ExcludeGeos: []string{"CN", "RU"}},
		"MinCPUCoresEffective": {MinCPUCoresEffective: 32},
		"MinHostRAMGB":         {MinHostRAMGB: 256},
		"InstanceType":         {InstanceType: cloud.InstanceTypeInterruptible},
		"RunpodCloudType":      {RunpodCloudType: "community"},
	}

	baseKey := constraintKey(base, cloud.ProviderVastai)
	bv := reflect.ValueOf(base)
	ct := reflect.TypeOf(cloud.OfferConstraints{})
	for i := 0; i < ct.NumField(); i++ {
		field := ct.Field(i)
		if !field.IsExported() || field.Tag.Get("axis") == "-" {
			continue
		}
		variant, ok := variants[field.Name]
		if !ok {
			t.Errorf("OfferConstraints.%s has no constraintKey variant: decide how the "+
				"search cache key covers it and add a case here", field.Name)
			continue
		}
		fv := reflect.ValueOf(variant).FieldByName(field.Name)
		if reflect.DeepEqual(fv.Interface(), bv.FieldByName(field.Name).Interface()) {
			t.Errorf("variant %q does not differ from base; the test cannot detect a missing key field", field.Name)
			continue
		}
		mutated := base
		reflect.ValueOf(&mutated).Elem().FieldByName(field.Name).Set(fv)
		if constraintKey(mutated, cloud.ProviderVastai) == baseKey {
			t.Errorf("constraintKey ignores OfferConstraints.%s: two groups differing "+
				"only in it would share one cached provider search", field.Name)
		}
	}

	if constraintKey(base, cloud.ProviderVastai) == constraintKey(base, cloud.ProviderRunpod) {
		t.Error("constraintKey ignores the requested provider")
	}
}

// Geo order must not split the cache: the same exclusion set spelled in a
// different order is the same search.
func TestConstraintKeyNormalizesGeoOrder(t *testing.T) {
	a := constraintKey(cloud.OfferConstraints{ExcludeGeos: []string{"CN", "RU"}}, cloud.ProviderVastai)
	b := constraintKey(cloud.OfferConstraints{ExcludeGeos: []string{"RU", "CN"}}, cloud.ProviderVastai)
	if a != b {
		t.Errorf("constraintKey depends on ExcludeGeos order: %q != %q", a, b)
	}
}
