package cache

import (
	"errors"
	"fmt"
	rediscli "github.com/go-redis/redis"
	goharborv1 "github.com/goharbor/harbor-cluster-operator/api/v1"
	"github.com/goharbor/harbor-cluster-operator/lcm"
	corev1 "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"
)

const (
	HarborChartMuseum = "chartMuseum"
	HarborClair       = "clair"
	HarborJobService  = "jobService"
	HarborRegistry    = "registry"
)

var (
	components = []string{
		HarborChartMuseum,
		HarborClair,
		HarborJobService,
		HarborRegistry,
	}
)

// Readiness reconcile will check Redis sentinel cluster if that has available.
// It does:
// - create redis connection pool
// - ping redis server
// - return redis properties if redis has available
func (redis *RedisReconciler) Readiness() (*lcm.CRStatus, error) {
	var (
		client *rediscli.Client
		err    error
	)

	switch redis.HarborCluster.Spec.Redis.Kind {
	case goharborv1.ExternalComponent:
		client, err = redis.GetExternalRedisInfo()
	case goharborv1.InClusterComponent:
		client, err = redis.GetInClusterRedisInfo()
	}

	if err != nil {
		redis.Log.Error(err, "Fail to create redis client.",
			"namespace", redis.HarborCluster.Namespace, "name", redis.HarborCluster.Name)
		return cacheNotReadyStatus(GetRedisClientError, err.Error()), err
	}

	defer client.Close()

	if err := client.Ping().Err(); err != nil {
		redis.Log.Error(err, "Fail to check Redis.",
			"namespace", redis.HarborCluster.Namespace, "name", redis.HarborCluster.Name)
		return cacheNotReadyStatus(CheckRedisHealthError, err.Error()), err
	}

	redis.Log.Info("Redis already ready.",
		"namespace", redis.HarborCluster.Namespace, "name", redis.HarborCluster.Name)

	properties := lcm.Properties{}
	for _, component := range components {
		url := redis.RedisConnect.GenRedisConnURL()
		secretName := fmt.Sprintf("%s-redis", strings.ToLower(component))
		propertyName := fmt.Sprintf("%sSecret", component)

		if err := redis.DeployComponentSecret(component, url, "", secretName); err != nil {
			return cacheNotReadyStatus(CreateComponentSecretError, err.Error()), err
		}

		properties.Add(propertyName, secretName)
	}

	return cacheReadyStatus(&properties), nil
}

// DeployComponentSecret deploy harbor component redis secret
func (redis *RedisReconciler) DeployComponentSecret(component, url, namespace, secretName string) error {
	secret := &corev1.Secret{}

	sc := redis.generateHarborCacheSecret(component, secretName, url, namespace)

	switch redis.HarborCluster.Spec.Redis.Kind {
	case goharborv1.ExternalComponent:
		if err := controllerutil.SetControllerReference(redis.HarborCluster, sc, redis.Scheme); err != nil {
			return err
		}
	case goharborv1.InClusterComponent:
		rf, err := redis.GetRedisFailover()
		if err != nil {
			return err
		}
		if err := controllerutil.SetControllerReference(rf, sc, redis.Scheme); err != nil {
			return err
		}
	}

	err := redis.Client.Get(types.NamespacedName{Name: secretName, Namespace: redis.HarborCluster.Namespace}, secret)
	if err != nil && kerr.IsNotFound(err) {
		redis.Log.Info("Creating Harbor Component Secret",
			"namespace", redis.HarborCluster.Namespace,
			"name", secretName,
			"component", component)
		return redis.Client.Create(sc)
	}

	return err
}

func (redis *RedisReconciler) GetExternalRedisInfo() (*rediscli.Client, error) {
	var (
		connect  *RedisConnect
		endpoint []string
		port     string
		client   *rediscli.Client
		err      error
		pw       string
	)
	spec := redis.HarborCluster.Spec.Redis.Spec
	switch spec.Schema {
	case RedisSentinelSchema:
		if len(spec.Hosts) < 1 || spec.GroupName == "" {
			return nil, errors.New(".redis.spec.hosts or .redis.spec.groupName is invalid")
		}

		endpoint, port = GetExternalRedisHost(spec)

		if spec.SecretName != "" {
			pw, err = redis.GetExternalRedisPassword(spec)
		}

		connect = &RedisConnect{
			Endpoints: endpoint,
			Port:      port,
			Password:  pw,
			GroupName: spec.GroupName,
			Schema:    RedisSentinelSchema,
		}

		redis.RedisConnect = connect
		client = connect.NewRedisPool()
	case RedisServerSchema:
		if len(spec.Hosts) != 1 {
			return nil, errors.New(".redis.spec.hosts is invalid")
		}
		endpoint, port = GetExternalRedisHost(spec)

		if spec.SecretName != "" {
			pw, err = redis.GetExternalRedisPassword(spec)
		}

		connect = &RedisConnect{
			Endpoints: endpoint,
			Port:      port,
			Password:  pw,
			GroupName: spec.GroupName,
			Schema:    RedisServerSchema,
		}
		redis.RedisConnect = connect
		client = connect.NewRedisClient()
	}

	if err != nil {
		return nil, err
	}

	return client, nil
}

// GetExternalRedisHost returns external redis host list and port
func GetExternalRedisHost(spec *goharborv1.RedisSpec) ([]string, string) {
	var (
		endpoint []string
		port     string
	)
	for _, host := range spec.Hosts {
		sp := host.Host
		endpoint = append(endpoint, sp)
		port = host.Port
	}
	return endpoint, port
}

// GetExternalRedisPassword returns external redis password
func (redis *RedisReconciler) GetExternalRedisPassword(spec *goharborv1.RedisSpec) (string, error) {

	pw, err := redis.GetRedisPassword(spec.SecretName)
	if err != nil {
		return "", err
	}

	return pw, err
}

// GetInClusterRedisInfo returns inCluster redis sentinel pool client
func (redis *RedisReconciler) GetInClusterRedisInfo() (*rediscli.Client, error) {
	password, err := redis.GetRedisPassword(redis.HarborCluster.Name)
	if err != nil {
		return nil, err
	}

	_, sentinelPodList, err := redis.GetDeploymentPods()
	if err != nil {
		redis.Log.Error(err, "Fail to get deployment pods.")
		return nil, err
	}

	_, redisPodList, err := redis.GetStatefulSetPods()
	if err != nil {
		redis.Log.Error(err, "Fail to get deployment pods.")
		return nil, err
	}

	if len(sentinelPodList.Items) == 0 || len(redisPodList.Items) == 0 {
		redis.Log.Info("pod list is empty，pls wait.")
		return nil, errors.New("pod list is empty，pls wait")
	}

	sentinelPodArray := sentinelPodList.Items

	_, currentSentinelPods := redis.GetPodsStatus(sentinelPodArray)

	if len(currentSentinelPods) == 0 {
		return nil, errors.New("need to requeue")
	}

	endpoint := redis.GetSentinelServiceUrl(currentSentinelPods)

	connect := &RedisConnect{
		Endpoints: []string{endpoint},
		Port:      RedisSentinelConnPort,
		Password:  password,
		GroupName: RedisSentinelConnGroup,
	}

	redis.RedisConnect = connect

	client := connect.NewRedisPool()

	return client, nil
}
