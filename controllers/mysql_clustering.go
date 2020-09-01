package controllers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cybozu-go/moco"
	mocov1alpha1 "github.com/cybozu-go/moco/api/v1alpha1"
	"github.com/go-logr/logr"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MySQLClusterStatus defines the observed state of MySQLCluster
type MySQLClusterStatus struct {
	InstanceStatus []MySQLInstanceStatus
}

// MySQLInstanceStatus defines the observed state of a MySQL instance
type MySQLInstanceStatus struct {
	Available            bool
	PrimaryStatus        *MySQLPrimaryStatus
	ReplicaStatus        *MySQLReplicaStatus
	GlobalVariableStatus *MySQLGlobalVariablesStatus
	CloneStateStatus     *MySQLCloneStateStatus
}

// MySQLPrimaryStatus defines the observed state of a primary
type MySQLPrimaryStatus struct {
	ExecutedGtidSet sql.NullString `db:"Executed_Gtid_Set"`
}

// MySQLReplicaStatus defines the observed state of a replica
type MySQLReplicaStatus struct {
	ID               int            `db:"id"`
	LastIoErrno      int            `db:"Last_IO_Errno"`
	LastIoError      sql.NullString `db:"Last_IO_Error"`
	LastSqlErrno     int            `db:"Last_SQL_Errno"`
	LastSqlError     sql.NullString `db:"Last_SQL_Error"`
	MasterHost       string         `db:"Master_Host"`
	RetrievedGtidSet sql.NullString `db:"Retrieved_Gtid_Set"`
	ExecutedGtidSet  sql.NullString `db:"Executed_Gtid_Set"`
	SlaveIoRunning   string         `db:"Slave_IO_Running"`
	SlaveSqlRunning  string         `db:"Slave_SQL_Running"`
}

// MySQLGlobalVariablesStatus defines the observed global variable state of a MySQL instance
type MySQLGlobalVariablesStatus struct {
	ReadOnly                           bool `db:"@@read_only"`
	SuperReadOnly                      bool `db:"@@super_read_only"`
	RplSemiSyncMasterWaitForSlaveCount int  `db:"@@rpl_semi_sync_master_wait_for_slave_count"`
}

// MySQLCloneStateStatus defines the observed clone state of a MySQL instance
type MySQLCloneStateStatus struct {
	State sql.NullString `db:"state"`
}

// Operator is the interface for operations
type Operator interface {
	Name() string
	Run(ctx context.Context, infra infrastructure, cluster *mocov1alpha1.MySQLCluster, status *MySQLClusterStatus) error
}

// Operation defines operations to MySQL Cluster
type Operation struct {
	Operators      []Operator
	Wait           bool
	Conditions     []mocov1alpha1.MySQLClusterCondition
	SyncedReplicas *int
}

// reconcileMySQLCluster reconciles MySQL cluster
func (r *MySQLClusterReconciler) reconcileClustering(ctx context.Context, log logr.Logger, cluster *mocov1alpha1.MySQLCluster) (ctrl.Result, error) {
	infra := infrastructure{r.Client, r.MySQLAccessor}
	status := r.getMySQLClusterStatus(ctx, log, infra, cluster)

	op, err := decideNextOperation(log, cluster, status)
	if err != nil {
		condErr := r.setFailureCondition(ctx, cluster, err, nil)
		if condErr != nil {
			log.Error(condErr, "unable to update status")
		}
		return ctrl.Result{}, err
	}

	for _, o := range op.Operators {
		err = o.Run(ctx, infra, cluster, status)
		if err != nil {
			condErr := r.setFailureCondition(ctx, cluster, err, nil)
			if condErr != nil {
				log.Error(condErr, "unable to update status")
			}
			return ctrl.Result{}, err
		}
	}
	err = r.setMySQLClusterStatus(ctx, cluster, op.Conditions, op.SyncedReplicas)

	if err == nil && op.Wait {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, err
}

func decideNextOperation(log logr.Logger, cluster *mocov1alpha1.MySQLCluster, status *MySQLClusterStatus) (*Operation, error) {
	var unavailable bool
	for i, is := range status.InstanceStatus {
		if !is.Available {
			log.Info("unavailable host exists", "index", i)
			unavailable = true
		}
	}
	if unavailable {
		return nil, moco.ErrUnavailableHost
	}
	log.Info("MySQLClusterStatus", "ClusterStatus", status)

	err := validateConstraints(status, cluster)
	if err != nil {
		return &Operation{
			Conditions: violationCondition(err),
		}, err
	}

	primaryIndex := selectPrimary(status, cluster)

	ops := updatePrimary(cluster, primaryIndex)
	if len(ops) != 0 {
		return &Operation{
			Operators: ops,
		}, nil
	}

	ops = configureReplication(status, cluster)
	if len(ops) != 0 {
		return &Operation{
			Operators: ops,
		}, nil
	}

	wait, outOfSyncInts := waitForReplication(status, cluster)
	if wait {
		return &Operation{
			Wait:       true,
			Conditions: unavailableCondition(outOfSyncInts),
		}, nil
	}

	syncedReplicas := int(cluster.Spec.Replicas) - len(outOfSyncInts)
	ops = acceptWriteRequest(status, cluster)
	if len(ops) != 0 {
		return &Operation{
			Conditions:     availableCondition(outOfSyncInts),
			Operators:      ops,
			SyncedReplicas: &syncedReplicas,
		}, nil
	}

	return &Operation{
		Conditions:     availableCondition(outOfSyncInts),
		SyncedReplicas: &syncedReplicas,
	}, nil
}

func (r *MySQLClusterReconciler) setFailureCondition(ctx context.Context, cluster *mocov1alpha1.MySQLCluster, e error, outOfSyncInstances []int) error {
	setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
		Type:    mocov1alpha1.ConditionFailure,
		Status:  corev1.ConditionTrue,
		Message: e.Error(),
	})
	setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionAvailable,
		Status: corev1.ConditionFalse,
	})
	setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionHealthy,
		Status: corev1.ConditionFalse,
	})
	if len(outOfSyncInstances) != 0 {
		msg := fmt.Sprintf("outOfSync instances: %#v", outOfSyncInstances)
		setCondition(&cluster.Status.Conditions, mocov1alpha1.MySQLClusterCondition{
			Type:    mocov1alpha1.ConditionOutOfSync,
			Status:  corev1.ConditionTrue,
			Message: msg,
		})
	}

	err := r.Status().Update(ctx, cluster)
	if err != nil {
		return err
	}
	return nil
}

func violationCondition(e error) []mocov1alpha1.MySQLClusterCondition {
	var conditions []mocov1alpha1.MySQLClusterCondition
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:    mocov1alpha1.ConditionViolation,
		Status:  corev1.ConditionTrue,
		Message: e.Error(),
	})
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionFailure,
		Status: corev1.ConditionTrue,
	})
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionAvailable,
		Status: corev1.ConditionFalse,
	})
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionHealthy,
		Status: corev1.ConditionFalse,
	})
	return conditions
}

func unavailableCondition(outOfSyncInstances []int) []mocov1alpha1.MySQLClusterCondition {
	var conditions []mocov1alpha1.MySQLClusterCondition
	if len(outOfSyncInstances) == 0 {
		setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
			Type:   mocov1alpha1.ConditionOutOfSync,
			Status: corev1.ConditionFalse,
		})
	} else {
		msg := fmt.Sprintf("outOfSync instances: %#v", outOfSyncInstances)
		setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
			Type:    mocov1alpha1.ConditionOutOfSync,
			Status:  corev1.ConditionTrue,
			Message: msg,
		})
	}
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionFailure,
		Status: corev1.ConditionFalse,
	})
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionHealthy,
		Status: corev1.ConditionFalse,
	})
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionAvailable,
		Status: corev1.ConditionFalse,
	})

	return conditions
}

func availableCondition(outOfSyncInstances []int) []mocov1alpha1.MySQLClusterCondition {
	var conditions []mocov1alpha1.MySQLClusterCondition
	if len(outOfSyncInstances) == 0 {
		setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
			Type:   mocov1alpha1.ConditionOutOfSync,
			Status: corev1.ConditionFalse,
		})
		setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
			Type:   mocov1alpha1.ConditionHealthy,
			Status: corev1.ConditionTrue,
		})
	} else {
		msg := fmt.Sprintf("outOfSync instances: %#v", outOfSyncInstances)
		setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
			Type:    mocov1alpha1.ConditionOutOfSync,
			Status:  corev1.ConditionTrue,
			Message: msg,
		})
		setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
			Type:   mocov1alpha1.ConditionHealthy,
			Status: corev1.ConditionFalse,
		})
	}
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionFailure,
		Status: corev1.ConditionFalse,
	})
	setCondition(&conditions, mocov1alpha1.MySQLClusterCondition{
		Type:   mocov1alpha1.ConditionAvailable,
		Status: corev1.ConditionTrue,
	})

	return conditions
}

func (r *MySQLClusterReconciler) setMySQLClusterStatus(ctx context.Context, cluster *mocov1alpha1.MySQLCluster, conditions []mocov1alpha1.MySQLClusterCondition, syncedStatus *int) error {
	for _, cond := range conditions {
		if cond.Type == mocov1alpha1.ConditionAvailable {
			cluster.Status.Ready = cond.Status
		}
		setCondition(&cluster.Status.Conditions, cond)
	}
	if syncedStatus != nil {
		cluster.Status.SyncedReplicas = *syncedStatus
	}
	err := r.Status().Update(ctx, cluster)
	if err != nil {
		return err
	}
	return nil
}

func (r *MySQLClusterReconciler) getMySQLClusterStatus(ctx context.Context, log logr.Logger, infra infrastructure, cluster *mocov1alpha1.MySQLCluster) *MySQLClusterStatus {
	status := &MySQLClusterStatus{
		InstanceStatus: make([]MySQLInstanceStatus, int(cluster.Spec.Replicas)),
	}
	for instanceIdx := 0; instanceIdx < int(cluster.Spec.Replicas); instanceIdx++ {
		status.InstanceStatus[instanceIdx].Available = false

		podName := fmt.Sprintf("%s-%d", uniqueName(cluster), instanceIdx)

		db, err := infra.getDB(ctx, cluster, instanceIdx)
		if err != nil {
			log.Info("instance not available", "err", err, "podName", podName)
			continue
		}

		primaryStatus, err := r.getMySQLPrimaryStatus(ctx, log, db)
		if err != nil {
			log.Info("get primary status failed", "err", err, "podName", podName)
			continue
		}
		status.InstanceStatus[instanceIdx].PrimaryStatus = primaryStatus

		replicaStatus, err := r.getMySQLReplicaStatus(ctx, log, db)
		if err != nil {
			log.Info("get replica status failed", "err", err, "podName", podName)
			continue
		}
		status.InstanceStatus[instanceIdx].ReplicaStatus = replicaStatus

		readOnlyStatus, err := r.getMySQLGlobalVariablesStatus(ctx, log, db)
		if err != nil {
			log.Info("get readOnly status failed", "err", err, "podName", podName)
			continue
		}
		status.InstanceStatus[instanceIdx].GlobalVariableStatus = readOnlyStatus

		cloneStatus, err := r.getMySQLCloneStateStatus(ctx, log, db)
		if err != nil {
			log.Info("get clone status failed", "err", err, "podName", podName)
			continue
		}
		status.InstanceStatus[instanceIdx].CloneStateStatus = cloneStatus

		status.InstanceStatus[instanceIdx].Available = true
	}
	return status
}

func (r *MySQLClusterReconciler) getMySQLPrimaryStatus(ctx context.Context, log logr.Logger, db *sqlx.DB) (*MySQLPrimaryStatus, error) {
	rows, err := db.Unsafe().Queryx(`SHOW MASTER STATUS`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var status MySQLPrimaryStatus
	if rows.Next() {
		err = rows.StructScan(&status)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}

	return nil, errors.New("primary status is empty")
}

func (r *MySQLClusterReconciler) getMySQLReplicaStatus(ctx context.Context, log logr.Logger, db *sqlx.DB) (*MySQLReplicaStatus, error) {
	rows, err := db.Unsafe().Queryx(`SHOW SLAVE STATUS`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var status MySQLReplicaStatus
	if rows.Next() {
		err = rows.StructScan(&status)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}

	return nil, nil
}

func (r *MySQLClusterReconciler) getMySQLGlobalVariablesStatus(ctx context.Context, log logr.Logger, db *sqlx.DB) (*MySQLGlobalVariablesStatus, error) {
	rows, err := db.Queryx(`SELECT @@read_only, @@super_read_only, @@rpl_semi_sync_master_wait_for_slave_count`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var status MySQLGlobalVariablesStatus
	if rows.Next() {
		err = rows.StructScan(&status)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}

	return nil, errors.New("readOnly status is empty")
}

func (r *MySQLClusterReconciler) getMySQLCloneStateStatus(ctx context.Context, log logr.Logger, db *sqlx.DB) (*MySQLCloneStateStatus, error) {
	rows, err := db.Queryx(`SELECT state FROM performance_schema.clone_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var status MySQLCloneStateStatus
	if rows.Next() {
		err = rows.StructScan(&status)
		if err != nil {
			return nil, err
		}
		return &status, nil
	}

	return nil, nil
}

func validateConstraints(status *MySQLClusterStatus, cluster *mocov1alpha1.MySQLCluster) error {
	if status == nil {
		panic("unreachable condition")
	}

	var writableInstanceCounts int
	var primaryIndex int
	for i, status := range status.InstanceStatus {
		if !status.GlobalVariableStatus.ReadOnly {
			writableInstanceCounts++
			primaryIndex = i
		}
	}
	if writableInstanceCounts > 1 {
		return moco.ErrConstraintsViolation
	}

	if cluster.Status.CurrentPrimaryIndex != nil && writableInstanceCounts == 1 {
		if *cluster.Status.CurrentPrimaryIndex != primaryIndex {
			return moco.ErrConstraintsViolation
		}
	}

	cond := findCondition(cluster.Status.Conditions, mocov1alpha1.ConditionViolation)
	if cond != nil {
		return moco.ErrConstraintsRecovered
	}

	return nil
}

// TODO: Implementation for failover
func selectPrimary(status *MySQLClusterStatus, cluster *mocov1alpha1.MySQLCluster) int {
	return 0
}

func updatePrimary(cluster *mocov1alpha1.MySQLCluster, newPrimaryIndex int) []Operator {
	currentPrimaryIndex := cluster.Status.CurrentPrimaryIndex
	if currentPrimaryIndex != nil && *currentPrimaryIndex == newPrimaryIndex {
		return nil
	}

	return []Operator{
		&updatePrimaryOp{
			newPrimaryIndex: newPrimaryIndex,
		},
	}
}

type infrastructure struct {
	client.Client
	MySQLAccessor *MySQLAccessor
}

func (i infrastructure) getClient() client.Client {
	return i.Client
}

func (i infrastructure) getDB(ctx context.Context, cluster *mocov1alpha1.MySQLCluster, index int) (*sqlx.DB, error) {
	operatorPassword, err := i.getPassword(ctx, cluster, moco.OperatorPasswordKey)
	if err != nil {
		return nil, err
	}

	db, err := i.MySQLAccessor.Get(getHost(cluster, index), moco.OperatorAdminUser, operatorPassword)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func (i infrastructure) getPassword(ctx context.Context, cluster *mocov1alpha1.MySQLCluster, passwordKey string) (string, error) {
	secret := &corev1.Secret{}
	myNS, mySecretName := getSecretNameForController(cluster)
	err := i.Get(ctx, client.ObjectKey{Namespace: myNS, Name: mySecretName}, secret)
	if err != nil {
		return "", err
	}
	return string(secret.Data[passwordKey]), nil
}

type updatePrimaryOp struct {
	newPrimaryIndex int
}

func (o *updatePrimaryOp) Name() string {
	return moco.OperatorUpdatePrimary
}

func (o *updatePrimaryOp) Run(ctx context.Context, infra infrastructure, cluster *mocov1alpha1.MySQLCluster, status *MySQLClusterStatus) error {
	db, err := infra.getDB(ctx, cluster, o.newPrimaryIndex)
	if err != nil {
		return err
	}
	cluster.Status.CurrentPrimaryIndex = &o.newPrimaryIndex
	err = infra.getClient().Status().Update(ctx, cluster)
	if err != nil {
		return err
	}

	_, err = db.Exec("SET GLOBAL rpl_semi_sync_master_enabled=ON,GLOBAL rpl_semi_sync_slave_enabled=OFF")
	if err != nil {
		return err
	}

	expectedRplSemiSyncMasterWaitForSlaveCount := int(cluster.Spec.Replicas / 2)
	st := status.InstanceStatus[o.newPrimaryIndex]
	if st.GlobalVariableStatus.RplSemiSyncMasterWaitForSlaveCount == expectedRplSemiSyncMasterWaitForSlaveCount {
		return nil
	}
	_, err = db.Exec("SET GLOBAL rpl_semi_sync_master_wait_for_slave_count=?", expectedRplSemiSyncMasterWaitForSlaveCount)
	return err
}

func configureReplication(status *MySQLClusterStatus, cluster *mocov1alpha1.MySQLCluster) []Operator {
	podName := fmt.Sprintf("%s-%d", uniqueName(cluster), *cluster.Status.CurrentPrimaryIndex)
	primaryHost := fmt.Sprintf("%s.%s.%s.svc", podName, uniqueName(cluster), cluster.Namespace)

	var operators []Operator
	for i, is := range status.InstanceStatus {
		if i == *cluster.Status.CurrentPrimaryIndex {
			continue
		}
		if is.ReplicaStatus == nil || is.ReplicaStatus.MasterHost != primaryHost {
			operators = append(operators, &configureReplicationOp{
				index:       i,
				primaryHost: primaryHost,
			})
		}
	}

	return operators
}

type configureReplicationOp struct {
	index       int
	primaryHost string
}

func (r configureReplicationOp) Name() string {
	return moco.OperatorConfigureReplication
}

func (r configureReplicationOp) Run(ctx context.Context, infra infrastructure, cluster *mocov1alpha1.MySQLCluster, status *MySQLClusterStatus) error {
	password, err := infra.getPassword(ctx, cluster, moco.ReplicationPasswordKey)
	if err != nil {
		return err
	}
	db, err := infra.getDB(ctx, cluster, r.index)
	if err != nil {
		return err
	}

	_, err = db.Exec(`STOP SLAVE`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CHANGE MASTER TO MASTER_HOST = ?, MASTER_PORT = ?, MASTER_USER = ?, MASTER_PASSWORD = ?, MASTER_AUTO_POSITION = 1`,
		r.primaryHost, moco.MySQLPort, moco.ReplicatorUser, password)
	if err != nil {
		return err
	}
	_, err = db.Exec("SET GLOBAL rpl_semi_sync_master_enabled=OFF,GLOBAL rpl_semi_sync_slave_enabled=ON")
	if err != nil {
		return err
	}
	_, err = db.Exec(`START SLAVE`)
	return err
}

func waitForReplication(status *MySQLClusterStatus, cluster *mocov1alpha1.MySQLCluster) (bool, []int) {
	primaryIndex := *cluster.Status.CurrentPrimaryIndex
	primaryStatus := status.InstanceStatus[primaryIndex]

	primaryGTID := primaryStatus.PrimaryStatus.ExecutedGtidSet
	count := 0
	var outOfSyncIns []int
	for i, is := range status.InstanceStatus {
		if i == primaryIndex {
			continue
		}

		if is.ReplicaStatus.LastIoErrno != 0 {
			outOfSyncIns = append(outOfSyncIns, i)
			continue
		}

		if is.ReplicaStatus.ExecutedGtidSet == primaryGTID {
			count++
		}
	}

	if !primaryStatus.GlobalVariableStatus.ReadOnly {
		return false, outOfSyncIns
	}

	return count < int(cluster.Spec.Replicas/2), outOfSyncIns
}

func acceptWriteRequest(status *MySQLClusterStatus, cluster *mocov1alpha1.MySQLCluster) []Operator {
	primaryIndex := *cluster.Status.CurrentPrimaryIndex

	if !status.InstanceStatus[primaryIndex].GlobalVariableStatus.ReadOnly {
		return nil
	}
	return []Operator{
		&turnOffReadOnlyOp{primaryIndex: primaryIndex}}
}

type turnOffReadOnlyOp struct {
	primaryIndex int
}

func (o turnOffReadOnlyOp) Name() string {
	return moco.OperatorTurnOffReadOnly
}

func (o turnOffReadOnlyOp) Run(ctx context.Context, infra infrastructure, cluster *mocov1alpha1.MySQLCluster, status *MySQLClusterStatus) error {
	db, err := infra.getDB(ctx, cluster, o.primaryIndex)
	if err != nil {
		return err
	}
	_, err = db.Exec("set global read_only=0")
	return err
}
