/**
 * Copyright (c) 2018 Dell Inc., or its subsidiaries. All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package zk

import (
	"fmt"
	"reflect"
	"strconv"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/pravega/zookeeper-operator/pkg/apis/zookeeper/v1beta1"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
)

type zkPorts struct {
	Client int32
	Quorum int32
	Leader int32
}

func deploy(z *v1beta1.ZookeeperCluster) (err error) {
	var ports zkPorts

	for _, p := range z.Spec.Ports {
		if p.Name == "client" {
			ports.Client = p.ContainerPort
		} else if p.Name == "quorum" {
			ports.Quorum = p.ContainerPort
		} else if p.Name == "leader-election" {
			ports.Leader = p.ContainerPort
		}
	}

	cm := configMapName(z)

	err = sdk.Create(makeZkConfigMap(cm, ports, z))
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		logrus.Errorf("Failed to create zookeeper configmap : %v", err)
		return err
	}

	err = sdk.Create(makeZkSts(cm, ports, z))
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		logrus.Errorf("Failed to create zookeeper statefulset : %v", err)
		return err
	}

	err = sdk.Create(makeZkPdb(z))
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		logrus.Errorf("Failed to create zookeeper pod-disruption-budget : %v", err)
		return err
	}

	err = sdk.Create(makeZkClientSvc(ports, z))
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		logrus.Errorf("Failed to create zookeeper client service : %v", err)
		return err
	}

	err = sdk.Create(makeZkHeadlessSvc(ports, z))
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		logrus.Errorf("Failed to create zookeeper headless service : %v", err)
		return err
	}

	return nil
}

func configMapName(z *v1beta1.ZookeeperCluster) string {
	return fmt.Sprintf("%s-configmap", z.GetName())
}

func headlessSvcName(z *v1beta1.ZookeeperCluster) string {
	return fmt.Sprintf("%s-headless", z.GetName())
}

func headlessDomain(z *v1beta1.ZookeeperCluster) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", headlessSvcName(z), z.GetNamespace())
}

func makeZkSts(configMapName string, ports zkPorts, z *v1beta1.ZookeeperCluster) *appsv1.StatefulSet {
	sts := appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      z.GetName(),
			Namespace: z.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(z, schema.GroupVersionKind{
					Group:   v1beta1.SchemeGroupVersion.Group,
					Version: v1beta1.SchemeGroupVersion.Version,
					Kind:    "ZookeeperCluster",
				}),
			},
			Labels: z.Spec.Labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: headlessSvcName(z),
			Replicas:    &z.Spec.Size,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": z.GetName(),
				},
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			PodManagementPolicy: appsv1.OrderedReadyPodManagement,
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: z.GetName(),
					Labels: map[string]string{
						"app":  z.GetName(),
						"kind": "ZookeeperMember",
					},
				},
				Spec: makeZkPodSpec(configMapName, ports, z),
			},
			VolumeClaimTemplates: []v1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "data",
						Labels: map[string]string{"app": z.GetName()},
					},
					Spec: *z.Spec.PersistentVolumeClaimSpec,
				},
			},
		},
	}
	return &sts
}

func makeZkPodSpec(configMapName string, ports zkPorts, z *v1beta1.ZookeeperCluster) v1.PodSpec {
	zkContainer := v1.Container{
		Name:            "zookeeper",
		Image:           z.Spec.Image.ToString(),
		Ports:           z.Spec.Ports,
		ImagePullPolicy: z.Spec.Image.PullPolicy,
		ReadinessProbe: &v1.Probe{
			InitialDelaySeconds: 10,
			TimeoutSeconds:      10,
			Handler: v1.Handler{
				Exec: &v1.ExecAction{Command: []string{"zookeeperReady.sh"}},
			},
		},
		LivenessProbe: &v1.Probe{
			InitialDelaySeconds: 10,
			TimeoutSeconds:      10,
			Handler: v1.Handler{
				Exec: &v1.ExecAction{Command: []string{"zookeeperLive.sh"}},
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "conf", MountPath: "/conf"},
		},
		Lifecycle: &v1.Lifecycle{
			PreStop: &v1.Handler{
				Exec: &v1.ExecAction{
					Command: []string{"zookeeperTeardown.sh"},
				},
			},
		},
		Command: []string{"/usr/local/bin/zookeeperStart.sh"},
	}
	if z.Spec.Pod.Resources.Limits != nil || z.Spec.Pod.Resources.Requests != nil {
		zkContainer.Resources = z.Spec.Pod.Resources
	}
	zkContainer.Env = z.Spec.Pod.Env
	podSpec := v1.PodSpec{
		Containers: []v1.Container{zkContainer},
		Affinity:   z.Spec.Pod.Affinity,
		Volumes: []v1.Volume{
			{
				Name: "conf",
				VolumeSource: v1.VolumeSource{
					ConfigMap: &v1.ConfigMapVolumeSource{
						LocalObjectReference: v1.LocalObjectReference{Name: configMapName},
					},
				},
			},
		},
		TerminationGracePeriodSeconds: &z.Spec.Pod.TerminationGracePeriodSeconds,
	}
	if reflect.DeepEqual(v1.PodSecurityContext{}, z.Spec.Pod.SecurityContext) {
		podSpec.SecurityContext = z.Spec.Pod.SecurityContext
	}
	podSpec.NodeSelector = z.Spec.Pod.NodeSelector
	podSpec.Tolerations = z.Spec.Pod.Tolerations

	return podSpec
}

func makeZkClientSvc(ports zkPorts, z *v1beta1.ZookeeperCluster) *v1.Service {
	name := fmt.Sprintf("%s-client", z.GetName())
	svcPorts := []v1.ServicePort{
		{Name: "client", Port: ports.Client},
	}
	return makeSvc(name, svcPorts, true, z)
}

func makeZkHeadlessSvc(ports zkPorts, z *v1beta1.ZookeeperCluster) *v1.Service {
	svcPorts := []v1.ServicePort{
		{Name: "quorum", Port: ports.Quorum},
		{Name: "leader-election", Port: ports.Leader},
	}
	return makeSvc(headlessSvcName(z), svcPorts, false, z)
}

func makeZkConfigMap(name string, ports zkPorts, z *v1beta1.ZookeeperCluster) *v1.ConfigMap {
	return &v1.ConfigMap{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ConfigMap",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: z.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(z, schema.GroupVersionKind{
					Group:   v1beta1.SchemeGroupVersion.Group,
					Version: v1beta1.SchemeGroupVersion.Version,
					Kind:    "ZookeeperCluster",
				}),
			},
		},
		Data: map[string]string{
			"zoo.cfg":                makeZkConfigString(ports, z),
			"log4j.properties":       makeZkLog4JConfigString(),
			"log4j-quiet.properties": makeZkLog4JQuietConfigString(),
			"env.sh":                 makeZkEnvConfigString(ports, z),
		},
	}
}

func makeZkConfigString(ports zkPorts, z *v1beta1.ZookeeperCluster) string {
	return "4lw.commands.whitelist=cons, envi, conf, crst, srvr, stat, mntr, ruok\n" +
		"dataDir=/data\n" +
		"standaloneEnabled=false\n" +
		"reconfigEnabled=true\n" +
		"skipACL=yes\n" +
		"initLimit=" + strconv.Itoa(z.Spec.Conf.InitLimit) + "\n" +
		"syncLimit=" + strconv.Itoa(z.Spec.Conf.SyncLimit) + "\n" +
		"tickTime=" + strconv.Itoa(z.Spec.Conf.TickTime) + "\n" +
		"dynamicConfigFile=/data/zoo.cfg.dynamic\n"
}

func makeZkLog4JQuietConfigString() string {
	return "log4j.rootLogger=ERROR, CONSOLE\n" +
		"log4j.appender.CONSOLE=org.apache.log4j.ConsoleAppender\n" +
		"log4j.appender.CONSOLE.Threshold=ERROR\n" +
		"log4j.appender.CONSOLE.layout=org.apache.log4j.PatternLayout\n" +
		"log4j.appender.CONSOLE.layout.ConversionPattern=%d{ISO8601} [myid:%X{myid}] - %-5p [%t:%C{1}@%L] - %m%n\n"
}

func makeZkLog4JConfigString() string {
	return "zookeeper.root.logger=CONSOLE\n" +
		"zookeeper.console.threshold=INFO\n" +
		"log4j.rootLogger=${zookeeper.root.logger}\n" +
		"log4j.appender.CONSOLE=org.apache.log4j.ConsoleAppender\n" +
		"log4j.appender.CONSOLE.Threshold=${zookeeper.console.threshold}\n" +
		"log4j.appender.CONSOLE.layout=org.apache.log4j.PatternLayout\n" +
		"log4j.appender.CONSOLE.layout.ConversionPattern=%d{ISO8601} [myid:%X{myid}] - %-5p [%t:%C{1}@%L] - %m%n\n"
}

func makeZkEnvConfigString(ports zkPorts, z *v1beta1.ZookeeperCluster) string {
	return "#!/usr/bin/env bash\n\n" +
		"DOMAIN=" + headlessDomain(z) + "\n" +
		"QUORUM_PORT=" + strconv.Itoa(int(ports.Quorum)) + "\n" +
		"LEADER_PORT=" + strconv.Itoa(int(ports.Leader)) + "\n" +
		"CLIENT_PORT=" + strconv.Itoa(int(ports.Client)) + "\n"
}

func makeSvc(name string, ports []v1.ServicePort, clusterIP bool, z *v1beta1.ZookeeperCluster) *v1.Service {
	service := v1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: z.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(z, schema.GroupVersionKind{
					Group:   v1beta1.SchemeGroupVersion.Group,
					Version: v1beta1.SchemeGroupVersion.Version,
					Kind:    "ZookeeperCluster",
				}),
			},
			Labels: map[string]string{"app": z.GetName()},
		},
		Spec: v1.ServiceSpec{
			Ports:    ports,
			Selector: map[string]string{"app": z.GetName()},
		},
	}
	if clusterIP == false {
		service.Spec.ClusterIP = v1.ClusterIPNone
	}
	return &service
}

func makeZkPdb(z *v1beta1.ZookeeperCluster) *policyv1beta1.PodDisruptionBudget {
	pdbCount := intstr.FromInt(1)
	return &policyv1beta1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PodDisruptionBudget",
			APIVersion: "policy/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      z.GetName(),
			Namespace: z.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(z, schema.GroupVersionKind{
					Group:   v1beta1.SchemeGroupVersion.Group,
					Version: v1beta1.SchemeGroupVersion.Version,
					Kind:    "ZookeeperCluster",
				}),
			},
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			MaxUnavailable: &pdbCount,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": z.GetName(),
				},
			},
		},
	}
}
