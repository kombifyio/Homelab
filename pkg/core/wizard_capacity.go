package core

// WizardServerCapacity is planning input, never host attestation or allocation authority.
type WizardServerCapacity struct {
	RAMMB    int `json:"ram_mb"`
	CPUCores int `json:"cpu_cores,omitempty"`
	DiskGB   int `json:"disk_gb,omitempty"`
}

type WizardCapacityUseCase struct {
	Goal                  string `json:"goal"`
	ResidentRAMMB         int    `json:"resident_ram_mb"`
	WorkingIncrementRAMMB *int   `json:"working_increment_ram_mb"`
	RequiredRAMMB         int    `json:"required_ram_mb"`
}

// WizardCapacityDriver names a concrete use case or setting choice that
// materially changes capacity demand (for Goals Configure explanations).
type WizardCapacityDriver struct {
	Goal    string `json:"goal"`
	Code    string `json:"code"`
	Setting string `json:"setting,omitempty"`
	Value   string `json:"value,omitempty"`
	RAMMB   int    `json:"ram_mb,omitempty"`
}

type WizardCapacityAssessment struct {
	Status                 string                       `json:"status"`
	Advisory               bool                         `json:"advisory"`
	Source                 string                       `json:"source"`
	Model                  string                       `json:"model"`
	AvailableRAMMB         int                          `json:"available_ram_mb"`
	RequiredRAMMB          int                          `json:"required_ram_mb"`
	ResidentRAMMB          int                          `json:"resident_ram_mb"`
	WorkingIncrementRAMMB  *int                         `json:"working_increment_ram_mb"`
	ConcurrentWorkloads    int                          `json:"concurrent_workloads"`
	SuggestedOfferingRAMMB int                          `json:"suggested_offering_ram_mb,omitempty"`
	SuggestedOfferingID    string                       `json:"suggested_offering_id,omitempty"`
	SuggestedRAMMB         int                          `json:"suggested_ram_mb"`
	OmitGoals              []string                     `json:"omit_goals"`
	Reasons                []WizardRecommendationReason `json:"reasons"`
	PerUseCase             []WizardCapacityUseCase      `json:"per_use_case"`
	Drivers                []WizardCapacityDriver       `json:"drivers,omitempty"`
}
