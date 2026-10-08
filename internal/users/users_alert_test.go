package users

import (
	"testing"

	btypes "github.com/aws/aws-sdk-go-v2/service/budgets/types"
)

func TestIsAlertDefaultThresholdType(t *testing.T) {
	n := btypes.Notification{NotificationType: btypes.NotificationTypeActual,
		ComparisonOperator: btypes.ComparisonOperatorGreaterThan, Threshold: 50}
	if !isAlert(n) {
		t.Fatal("a notification without ThresholdType (AWS omits the PERCENTAGE default) is an alert")
	}
	n.ThresholdType = btypes.ThresholdTypeAbsoluteValue
	if isAlert(n) {
		t.Fatal("an absolute-value notification is not an alert")
	}
}
