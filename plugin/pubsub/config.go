package pubsub

type CloudConfig struct {
	Pubsub struct {
		Size int `yaml:"size" json:"size" default:"100"`
	} `yaml:"defaultpubsub" json:"defaultpubsub"`
}
