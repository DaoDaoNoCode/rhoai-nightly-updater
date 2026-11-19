import { describe, expect, it } from "vitest";
import { describeError, describeOutcomeError } from "./errors";
import { outcomeTitle, outcomeVariant } from "./outcomes";
import { ApiError } from "./services/api";

// pkg/cluster UserTokenRejectedMessage, as a stream's final event carries it.
const MESSAGE =
  "Failed to create DataScienceCluster: k8s API error 401 (Unauthorized): your OpenShift sign-in is no longer accepted (it expired or was revoked), so the operation stopped here. Log out of the updater and log in again, then check what the operation changed before you retry";

describe("sign_in_ended (the user's sign-in ended while a change ran)", () => {
  it("says the sign-in ended during the change, not that the updater's credentials were rejected", () => {
    // A stream reports the outcome in its final event (HTTP 200).
    const d = describeOutcomeError({ message: MESSAGE, errorCode: "sign_in_ended", httpStatus: 200 }, "Create failed");
    expect(d).toMatchObject({ title: "Your sign-in ended during the change", variant: "danger", reload: true });
    expect(d.hint).toBe("Part of the change may have run. Sign in again, check the result, then run it again if it is not complete.");
    expect(`${d.title} ${d.body} ${d.hint}`).not.toMatch(/ServiceAccount|updater's credentials/);
    expect(outcomeTitle({ success: false, errorCode: "sign_in_ended", message: MESSAGE })).toBe("Stopped: your sign-in ended");
    expect(outcomeVariant({ success: false, errorCode: "sign_in_ended" })).toBe("danger");
  });

  it("keeps the other 401s apart", () => {
    // Refused before anything ran: the session expired, nothing changed.
    expect(describeError(new ApiError({ status: 401, errorCode: "session_expired", message: "Your OpenShift session has expired. Log in again." })))
      .toMatchObject({ title: "Your session expired", hint: "Sign in again to continue; nothing on the cluster was changed." });
    // The app's own ServiceAccount rejected (not the user's sign-in).
    expect(describeError(new ApiError({ status: 502, errorCode: "unauthorized", message: "rejected" })).title).toBe("The cluster rejected the updater's credentials");
  });
});
