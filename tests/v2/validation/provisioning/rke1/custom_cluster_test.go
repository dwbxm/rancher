package rke1

import (
	"testing"

	"github.com/rancher/rancher/tests/framework/clients/rancher"
	management "github.com/rancher/rancher/tests/framework/clients/rancher/generated/management/v3"
	"github.com/rancher/rancher/tests/framework/extensions/clusters"
	"github.com/rancher/rancher/tests/framework/extensions/clusters/kubernetesversions"
	"github.com/rancher/rancher/tests/framework/extensions/users"
	password "github.com/rancher/rancher/tests/framework/extensions/users/passwordgenerator"
	"github.com/rancher/rancher/tests/framework/pkg/config"
	namegen "github.com/rancher/rancher/tests/framework/pkg/namegenerator"
	"github.com/rancher/rancher/tests/framework/pkg/session"
	"github.com/rancher/rancher/tests/v2/validation/provisioning"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type CustomClusterProvisioningTestSuite struct {
	suite.Suite
	client             *rancher.Client
	session            *session.Session
	standardUserClient *rancher.Client
	kubernetesVersions []string
	cnis               []string
	nodeProviders      []string
	psact              string
}

func (c *CustomClusterProvisioningTestSuite) TearDownSuite() {
	c.session.Cleanup()
}

func (c *CustomClusterProvisioningTestSuite) SetupSuite() {
	testSession := session.NewSession()
	c.session = testSession

	clustersConfig := new(provisioning.Config)
	config.LoadConfig(provisioning.ConfigurationFileKey, clustersConfig)

	c.kubernetesVersions = clustersConfig.RKE1KubernetesVersions
	c.cnis = clustersConfig.CNIs
	c.nodeProviders = clustersConfig.NodeProviders
	c.psact = clustersConfig.PSACT

	client, err := rancher.NewClient("", testSession)
	require.NoError(c.T(), err)

	c.client = client

	c.kubernetesVersions, err = kubernetesversions.Default(c.client, clusters.RKE1ClusterType.String(), c.kubernetesVersions)
	require.NoError(c.T(), err)

	enabled := true
	var testuser = namegen.AppendRandomString("testuser-")
	var testpassword = password.GenerateUserPassword("testpass-")
	user := &management.User{
		Username: testuser,
		Password: testpassword,
		Name:     testuser,
		Enabled:  &enabled,
	}

	newUser, err := users.CreateUserWithRole(client, user, "user")
	require.NoError(c.T(), err)

	newUser.Password = user.Password

	standardUserClient, err := client.AsUser(newUser)
	require.NoError(c.T(), err)

	c.standardUserClient = standardUserClient
}

func (c *CustomClusterProvisioningTestSuite) TestProvisioningRKE1CustomCluster() {
	nodeRoles0 := []string{
		"--etcd --controlplane --worker",
	}

	nodeRoles1 := []string{
		"--etcd --controlplane",
		"--worker",
	}

	nodeRoles2 := []string{
		"--etcd",
		"--controlplane",
		"--worker",
	}

	tests := []struct {
		name      string
		nodeRoles []string
		client    *rancher.Client
		psact     string
	}{
		{"1 Node all roles " + provisioning.AdminClientName.String(), nodeRoles0, c.client, c.psact},
		{"1 Node all roles " + provisioning.StandardClientName.String(), nodeRoles0, c.standardUserClient, c.psact},
		{"2 nodes - etcd/cp roles per 1 node " + provisioning.AdminClientName.String(), nodeRoles1, c.client, c.psact},
		{"2 nodes - etcd/cp roles per 1 node " + provisioning.StandardClientName.String(), nodeRoles1, c.standardUserClient, c.psact},
		{"3 nodes - 1 role per node " + provisioning.AdminClientName.String(), nodeRoles2, c.client, c.psact},
		{"3 nodes - 1 role per node " + provisioning.StandardClientName.String(), nodeRoles2, c.standardUserClient, c.psact},
	}
	var name string
	for _, tt := range tests {
		testSession := session.NewSession()
		defer testSession.Cleanup()

		client, err := tt.client.WithSession(testSession)
		require.NoError(c.T(), err)

		for _, nodeProviderName := range c.nodeProviders {
			externalNodeProvider := provisioning.ExternalNodeProviderSetup(nodeProviderName)
			providerName := " Node Provider: " + nodeProviderName
			for _, kubeVersion := range c.kubernetesVersions {
				name = tt.name + providerName + " Kubernetes version: " + kubeVersion
				for _, cni := range c.cnis {
					name += " cni: " + cni
					c.Run(name, func() {
						TestProvisioningRKE1CustomCluster(c.T(), client, externalNodeProvider, tt.nodeRoles, tt.psact, kubeVersion, cni)
					})
				}
			}
		}
	}
}

func (c *CustomClusterProvisioningTestSuite) TestProvisioningRKE1CustomClusterDynamicInput() {
	rolesPerNode := []string{}

	clustersConfig := new(provisioning.Config)
	config.LoadConfig(provisioning.ConfigurationFileKey, clustersConfig)
	nodesAndRoles := clustersConfig.NodesAndRolesRKE1

	if len(nodesAndRoles) == 0 {
		c.T().Skip()
	}

	for _, nodes := range nodesAndRoles {
		var finalRoleCommand string
		if nodes.ControlPlane {
			finalRoleCommand += " --controlplane"
		}
		if nodes.Etcd {
			finalRoleCommand += " --etcd"
		}
		if nodes.Worker {
			finalRoleCommand += " --worker"
		}
		rolesPerNode = append(rolesPerNode, finalRoleCommand)
	}

	tests := []struct {
		name   string
		client *rancher.Client
		psact  string
	}{
		{provisioning.AdminClientName.String(), c.client, c.psact},
		{provisioning.StandardClientName.String(), c.standardUserClient, c.psact},
	}

	var name string
	for _, tt := range tests {
		testSession := session.NewSession()
		defer testSession.Cleanup()

		client, err := tt.client.WithSession(testSession)
		require.NoError(c.T(), err)

		for _, nodeProviderName := range c.nodeProviders {
			externalNodeProvider := provisioning.ExternalNodeProviderSetup(nodeProviderName)
			for _, kubeVersion := range c.kubernetesVersions {
				name = tt.name + " Kubernetes version: " + kubeVersion
				for _, cni := range c.cnis {
					name += " cni: " + cni
					c.Run(name, func() {
						TestProvisioningRKE1CustomCluster(c.T(), client, externalNodeProvider, rolesPerNode, tt.psact, kubeVersion, cni)
					})
				}
			}
		}
	}
}

// In order for 'go test' to run this suite, we need to create
// a normal test function and pass our suite to suite.Run
func TestCustomClusterRKE1ProvisioningTestSuite(t *testing.T) {
	suite.Run(t, new(CustomClusterProvisioningTestSuite))
}
