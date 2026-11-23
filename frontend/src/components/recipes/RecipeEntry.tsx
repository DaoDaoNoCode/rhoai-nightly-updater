import React, { useEffect, useState } from "react";
import { Button, Card, CardBody, CardFooter, CardHeader, CardTitle, Content, StackItem, Title } from "@patternfly/react-core";
import { Link, useNavigate } from "react-router-dom";
import { getRecipeRun } from "../../services/api";
import type { RecipeRun } from "../../types";
import { RelativeTime } from "../RelativeTime";

/**
 * The Status page's way in while RHOAI or the DataScienceCluster is
 * missing: a recipe sets up the rest in one run.
 */
export const RecipeEntryCard: React.FC<{ setupDone: boolean }> = ({ setupDone }) => {
  const navigate = useNavigate();
  return (
  <Card>
    <CardHeader><CardTitle><Title headingLevel="h2" size="lg">Set up this cluster from a recipe</Title></CardTitle></CardHeader>
    <CardBody>
      <Content component="p">
        One run installs an RHOAI nightly, creates the DataScienceCluster, a project, and the Environment features you choose, each
        only when it is missing. The review says how long it usually takes; you can leave the page while it runs.
        {!setupDone && " Finish the one-time cluster setup first: a recipe does not hold the pull secret."}
      </Content>
    </CardBody>
    <CardFooter>
      <Button variant="secondary" onClick={() => navigate("/recipes")}>Open Recipes</Button>
    </CardFooter>
  </Card>
  );
};

/**
 * The Environment Overview's line about the last recipe run: which recipe,
 * with which project, as a StackItem. Nothing when no run is recorded or it
 * cannot be read.
 */
export const RecipeRunNote: React.FC = () => {
  const [run, setRun] = useState<RecipeRun | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    getRecipeRun(controller.signal)
      .then((res) => { if (!controller.signal.aborted && res.run?.project) setRun(res.run); })
      .catch(() => { /* Optional: the Overview works without it. */ });
    return () => controller.abort();
  }, []);
  if (!run?.project) return null;
  return (
    <StackItem>
    <Content component="p">
      The last recipe run, <Link to="/recipes">recipe {run.recipe.metadata.name}</Link>, started <RelativeTime date={run.startedAt} size="inherit" />,
      {run.project.owned ? " created" : " used"} project <Link to={`?tab=pipelines&project=${encodeURIComponent(run.project.name)}`}>{run.project.name}</Link>.
    </Content>
    </StackItem>
  );
};
