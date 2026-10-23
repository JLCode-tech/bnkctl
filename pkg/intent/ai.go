package intent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var sageMakerInstanceTypeRE = regexp.MustCompile(`^ml\.[a-z][0-9a-z]+\.[a-z0-9]+$`)

var smInstanceGPUCount = map[string]int{
	"ml.g5.xlarge":   1,
	"ml.g5.2xlarge":  1,
	"ml.g5.4xlarge":  1,
	"ml.g5.8xlarge":  1,
	"ml.g5.16xlarge": 1,
	"ml.g5.12xlarge": 4,
	"ml.g5.24xlarge": 4,
	"ml.g5.48xlarge": 8,
	"ml.g6.xlarge":   1,
	"ml.g6.2xlarge":  1,
	"ml.g6.4xlarge":  1,
	"ml.g6.8xlarge":  1,
	"ml.g6.16xlarge": 1,
	"ml.g6.12xlarge": 4,
	"ml.g6.24xlarge": 4,
	"ml.g6.48xlarge": 8,
}

// DefaultSyntheticImage is the default container image used for simulated vLLM inference.
const DefaultSyntheticImage = "ghcr.io/llm-d/llm-d-inference-sim:v0.11.2"

// DefaultSyntheticProfile is the model size simulated when ai.synthetic.profile is unset.
const DefaultSyntheticProfile = "llama-3.1-8b"

// AISpec is the top-level opt-in AI inference block in cluster.yaml.
type AISpec struct {
	SageMaker *SageMakerSpec `yaml:"sagemaker,omitempty"`
	Synthetic *SyntheticSpec `yaml:"synthetic,omitempty"`
}

// SyntheticSpec configures the lightweight synthetic/simulated vLLM endpoint using llm-d-inference-sim.
type SyntheticSpec struct {
	Enabled             bool    `yaml:"enabled"`
	Image               string  `yaml:"image,omitempty"`
	Profile             string  `yaml:"profile,omitempty"`
	Model               string  `yaml:"model,omitempty"`
	ServedModelName     string  `yaml:"servedModelName,omitempty"`
	Replicas            int     `yaml:"replicas,omitempty"`
	TTFTBaseMs          int     `yaml:"ttftBaseMs,omitempty"`
	PrefillUsPerToken   int     `yaml:"prefillUsPerToken,omitempty"`
	ITLMs               float64 `yaml:"itlMs,omitempty"`
	MaxNumSeqs          int     `yaml:"maxNumSeqs,omitempty"`
	TimeFactorUnderLoad float64 `yaml:"timeFactorUnderLoad,omitempty"`
	MaxModelLen         int     `yaml:"maxModelLen,omitempty"`
	KVCacheBlocks       int     `yaml:"kvCacheBlocks,omitempty"`
}

// SyntheticProfiles holds the timing of each simulated model size.
var SyntheticProfiles = map[string]SyntheticSpec{
	"llama-3.1-8b": {
		Model: "NousResearch/Meta-Llama-3.1-8B-Instruct", TTFTBaseMs: 7, PrefillUsPerToken: 18, ITLMs: 4.6,
		MaxNumSeqs: 256, TimeFactorUnderLoad: 2.5, MaxModelLen: 131072, KVCacheBlocks: 7000,
	},
	"llama-3.3-70b": {
		Model: "unsloth/Llama-3.3-70B-Instruct", TTFTBaseMs: 14, PrefillUsPerToken: 45, ITLMs: 13.8,
		MaxNumSeqs: 256, TimeFactorUnderLoad: 2.0, MaxModelLen: 131072, KVCacheBlocks: 12000,
	},
	"llama-3.1-405b": {
		Model: "NousResearch/Hermes-3-Llama-3.1-405B", TTFTBaseMs: 30, PrefillUsPerToken: 120, ITLMs: 38,
		MaxNumSeqs: 128, TimeFactorUnderLoad: 2.0, MaxModelLen: 131072, KVCacheBlocks: 7000,
	},
}

// AIBackendSageMaker identifies the SageMaker HyperPod backend.
const AIBackendSageMaker = "sagemaker"

// SageMakerInstanceGroup configures one HyperPod instance group.
type SageMakerInstanceGroup struct {
	Name                    string            `yaml:"name"`
	InstanceType            string            `yaml:"instanceType"`
	InstanceCount           int               `yaml:"instanceCount"`
	VolumeSizeGiB           int               `yaml:"volumeSizeGiB,omitempty"`
	ThreadsPerCore          int               `yaml:"threadsPerCore,omitempty"`
	EnableStressCheck       bool              `yaml:"enableStressCheck,omitempty"`
	EnableConnectivityCheck bool              `yaml:"enableConnectivityCheck,omitempty"`
	Labels                  map[string]string `yaml:"labels,omitempty"`
}

// EffectiveVolumeSizeGiB returns the configured EBS volume size or 500 GiB by default.
func (ig SageMakerInstanceGroup) EffectiveVolumeSizeGiB() int {
	if ig.VolumeSizeGiB > 0 {
		return ig.VolumeSizeGiB
	}
	return 500
}

// SageMakerSpec configures the disposable SageMaker LMI endpoint or HyperPod cluster (AWS).
type SageMakerSpec struct {
	Enabled              bool                     `yaml:"enabled"`
	ClusterName          string                   `yaml:"clusterName,omitempty"`
	ExecutionRoleARN     string                   `yaml:"executionRoleArn,omitempty"`
	LifecycleScriptS3URI string                   `yaml:"lifecycleScriptS3Uri,omitempty"`
	NodeRecovery         string                   `yaml:"nodeRecovery,omitempty"`
	InstanceGroups       []SageMakerInstanceGroup `yaml:"instanceGroups,omitempty"`
	Model                string                   `yaml:"model,omitempty"`
	InstanceType         string                   `yaml:"instanceType,omitempty"`
	ScaleToZero          bool                     `yaml:"scaleToZero,omitempty"`
	ImageURI             string                   `yaml:"imageUri,omitempty"`
	TensorParallelSize   int                      `yaml:"tensorParallelSize,omitempty"`
	MaxModelLen          int                      `yaml:"maxModelLen,omitempty"`
}

// EffectiveClusterName returns the configured HyperPod cluster name or "<clusterName>-hyperpod".
func (s *SageMakerSpec) EffectiveClusterName(clusterName string) string {
	if s != nil && s.ClusterName != "" {
		return s.ClusterName
	}
	return clusterName + "-hyperpod"
}

// EffectiveNodeRecovery returns the configured node recovery mode or "Automatic".
func (s *SageMakerSpec) EffectiveNodeRecovery() string {
	if s != nil && s.NodeRecovery != "" {
		return s.NodeRecovery
	}
	return "Automatic"
}

// IsSageMakerHyperPod reports whether SageMaker HyperPod instance groups are configured.
func (a *AISpec) IsSageMakerHyperPod() bool {
	return a != nil && a.SageMaker != nil && a.SageMaker.Enabled && len(a.SageMaker.InstanceGroups) > 0
}

// SageMakerEnabled reports whether the SageMaker endpoint should be provisioned.
func (c *Cluster) SageMakerEnabled() bool {
	return c != nil && c.AI != nil && c.AI.SageMaker != nil && c.AI.SageMaker.Enabled
}

func validateSageMaker(s *SageMakerSpec) error {
	if s == nil || !s.Enabled {
		return nil
	}
	if s.Model == "" && len(s.InstanceGroups) == 0 {
		return fmt.Errorf("ai.sagemaker.model is required when ai.sagemaker.enabled is true")
	}
	if s.InstanceType != "" && !sageMakerInstanceTypeRE.MatchString(s.InstanceType) {
		return fmt.Errorf("ai.sagemaker.instanceType %q does not match expected pattern (e.g. ml.g5.2xlarge)", s.InstanceType)
	}
	if s.TensorParallelSize < 0 {
		return fmt.Errorf("ai.sagemaker.tensorParallelSize must be >= 0, got %d", s.TensorParallelSize)
	}
	if s.TensorParallelSize > 8 {
		return fmt.Errorf("ai.sagemaker.tensorParallelSize %d exceeds 8 (no current ml.g5/ml.g6 SKU has more than 8 GPUs)", s.TensorParallelSize)
	}
	if s.MaxModelLen < 0 {
		return fmt.Errorf("ai.sagemaker.maxModelLen must be >= 0, got %d", s.MaxModelLen)
	}
	if s.TensorParallelSize > 0 && s.InstanceType != "" {
		if gpuCount, known := smInstanceGPUCount[s.InstanceType]; known && s.TensorParallelSize > gpuCount {
			return fmt.Errorf("ai.sagemaker.tensorParallelSize %d exceeds GPU count for %s (%d GPU)", s.TensorParallelSize, s.InstanceType, gpuCount)
		}
	}
	return nil
}

func applySageMakerDefaults(s *SageMakerSpec) {
	if s.InstanceType == "" {
		s.InstanceType = "ml.g5.2xlarge"
	}
}

// SyntheticAIEnabled reports whether synthetic AI inference simulation is enabled.
func (c *Cluster) SyntheticAIEnabled() bool {
	return c != nil && c.AI != nil && c.AI.Synthetic != nil && c.AI.Synthetic.Enabled
}

// UsesSyntheticAI reports whether the AI scenario runs the simulator across AWS, Azure, and GCP.
func (c *Cluster) UsesSyntheticAI() bool {
	if c == nil {
		return false
	}
	if c.SyntheticAIEnabled() {
		return true
	}
	if c.ClusterSpec == nil {
		return false
	}
	switch c.EffectiveProvider() {
	case ProviderAzure:
		if c.ClusterSpec.SystemNodePool.VMSize == "" && c.ClusterSpec.TMMNodePool.VMSize == "" {
			return false
		}
		if strings.HasPrefix(strings.ToLower(c.ClusterSpec.SystemNodePool.VMSize), "standard_n") ||
			strings.HasPrefix(strings.ToLower(c.ClusterSpec.TMMNodePool.VMSize), "standard_n") {
			return false
		}
		return true
	case ProviderGCP:
		if c.ClusterSpec.DefaultPool.MachineType == "" && c.ClusterSpec.TmmPool.MachineType == "" {
			return false
		}
		mt := strings.ToLower(c.ClusterSpec.DefaultPool.MachineType)
		tmmMt := strings.ToLower(c.ClusterSpec.TmmPool.MachineType)
		if strings.HasPrefix(mt, "a2-") || strings.HasPrefix(mt, "g2-") || strings.HasPrefix(mt, "a3-") ||
			strings.HasPrefix(tmmMt, "a2-") || strings.HasPrefix(tmmMt, "g2-") || strings.HasPrefix(tmmMt, "a3-") {
			return false
		}
		return true
	default:
		if len(c.ClusterSpec.NodeGroups) == 0 {
			return false
		}
		for _, ng := range c.ClusterSpec.NodeGroups {
			if ng.IsGPU() {
				return false
			}
		}
		return true
	}
}

// SimulatorTags labels benchmark results with what the simulator stands in for.
func (c *Cluster) SimulatorTags() map[string]string {
	if !c.UsesSyntheticAI() {
		return nil
	}
	s := c.ResolvedSynthetic()
	return map[string]string{"model_server": "llm-d-inference-sim", "sim_profile": s.Profile, "sim_model": s.Model}
}

// ResolvedSynthetic returns ai.synthetic with every default applied.
func (c *Cluster) ResolvedSynthetic() SyntheticSpec {
	var s SyntheticSpec
	if c != nil && c.AI != nil && c.AI.Synthetic != nil {
		s = *c.AI.Synthetic
	}
	applySyntheticDefaults(&s)
	return s
}

func validateSynthetic(s *SyntheticSpec) error {
	if s == nil || !s.Enabled {
		return nil
	}
	if s.Profile != "" {
		if _, ok := SyntheticProfiles[s.Profile]; !ok {
			return fmt.Errorf("ai.synthetic.profile %q is not one of %s", s.Profile, strings.Join(SyntheticProfileNames(), ", "))
		}
	}
	for name, v := range map[string]float64{
		"replicas": float64(s.Replicas), "ttftBaseMs": float64(s.TTFTBaseMs), "prefillUsPerToken": float64(s.PrefillUsPerToken),
		"itlMs": s.ITLMs, "maxNumSeqs": float64(s.MaxNumSeqs), "maxModelLen": float64(s.MaxModelLen), "kvCacheBlocks": float64(s.KVCacheBlocks),
	} {
		if v < 0 {
			return fmt.Errorf("ai.synthetic.%s must be >= 0, got %v", name, v)
		}
	}
	if s.TimeFactorUnderLoad != 0 && s.TimeFactorUnderLoad < 1 {
		return fmt.Errorf("ai.synthetic.timeFactorUnderLoad must be >= 1, got %v", s.TimeFactorUnderLoad)
	}
	return nil
}

// SyntheticProfileNames lists the known ai.synthetic.profile values, sorted.
func SyntheticProfileNames() []string {
	names := make([]string, 0, len(SyntheticProfiles))
	for n := range SyntheticProfiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func applySyntheticDefaults(s *SyntheticSpec) {
	if s.Image == "" {
		s.Image = DefaultSyntheticImage
	}
	if s.Profile == "" {
		s.Profile = DefaultSyntheticProfile
	}
	p := SyntheticProfiles[s.Profile]
	if s.Model == "" {
		s.Model = p.Model
	}
	if s.ServedModelName == "" {
		s.ServedModelName = "llama3"
	}
	if s.Replicas <= 0 {
		s.Replicas = 1
	}
	if s.TTFTBaseMs <= 0 {
		s.TTFTBaseMs = p.TTFTBaseMs
	}
	if s.PrefillUsPerToken <= 0 {
		s.PrefillUsPerToken = p.PrefillUsPerToken
	}
	if s.ITLMs <= 0 {
		s.ITLMs = p.ITLMs
	}
	if s.MaxNumSeqs <= 0 {
		s.MaxNumSeqs = p.MaxNumSeqs
	}
	if s.TimeFactorUnderLoad <= 0 {
		s.TimeFactorUnderLoad = p.TimeFactorUnderLoad
	}
	if s.MaxModelLen <= 0 {
		s.MaxModelLen = p.MaxModelLen
	}
	if s.KVCacheBlocks <= 0 {
		s.KVCacheBlocks = p.KVCacheBlocks
	}
}
