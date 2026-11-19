/**
 * The name to show for the Deployment Dashboard Dev pauses. The backend
 * discovers it (the dashboard module's operator; dashboard-operator on RHOAI
 * 3.6) and reports it as operatorName; without it the text stays generic.
 */
export function dashboardOperatorLabel(operatorName?: string | null): string {
  return operatorName || "the dashboard operator";
}
