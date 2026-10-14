package main

import (
	"testing"

	"github.com/MoonWX/oss-transfer/internal/servicehost"
)

func TestPublishedAgentDefaultsAndExplicitMirrors(t *testing.T) {
	oldBase, oldVersion, oldImage := agentArtifactBase, agentArtifactVersion, agentDockerImage
	t.Cleanup(func() { agentArtifactBase, agentArtifactVersion, agentDockerImage = oldBase, oldVersion, oldImage })
	agentArtifactBase, agentArtifactVersion, agentDockerImage = "https://downloads.example/osstransfer", "v4.1.0", "moonwx/osstransfer:v4.1.0"
	config := servicehost.Config{}
	if err := configureAgentArtifacts(&config); err != nil || config.AgentArtifactVersion != "v4.1.0" || config.AgentArtifactBase != agentArtifactBase || config.AgentDockerImage != agentDockerImage {
		t.Fatalf("published binary has no matching Agent artifacts: %+v, %v", config, err)
	}
	config = servicehost.Config{AgentArtifactBase: "https://mirror.example/releases", AgentArtifactVersion: "v4.1.1", AgentDockerImage: "registry.example/transfer:v4.1.1"}
	if err := configureAgentArtifacts(&config); err != nil || config.AgentArtifactVersion != "v4.1.1" || config.AgentDockerImage != "registry.example/transfer:v4.1.1" {
		t.Fatalf("explicit mirror was overridden: %+v, %v", config, err)
	}
	for _, incomplete := range []servicehost.Config{{AgentArtifactBase: "https://mirror.example/releases"}, {AgentArtifactVersion: "v4.1.1"}, {AgentArtifactBase: "https://mirror.example", AgentArtifactVersion: "latest"}} {
		if configureAgentArtifacts(&incomplete) == nil {
			t.Fatal("incomplete or moving artifact identity accepted")
		}
	}
}
