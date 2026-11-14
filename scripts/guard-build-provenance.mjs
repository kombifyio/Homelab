#!/usr/bin/env node
import {execFileSync} from 'node:child_process';
import {readFileSync,statSync} from 'node:fs';
import {createHash} from 'node:crypto';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
export function guardBuildProvenance(directory,env=process.env) {
  if(!env.NATIVE_PROVENANCE)return null;
  const value=JSON.parse(env.NATIVE_PROVENANCE),binding=value.binding;
  const source=execFileSync('git',['rev-parse','HEAD'],{encoding:'utf8'}).trim();
  if(env.GITHUB_REPOSITORY!==binding?.parent?.repository||env.GITHUB_RUN_ATTEMPT!=='1'||env.RELEASE_MODE!=='publish'||
      !/^[a-f0-9]{40}$/.test(source)||source!==env.SOURCE_SHA||source!==binding?.plan?.source_sha||
      binding.schema!=='delivery.dispatch-binding/v1'||binding.plan.product_id!=='techstack'||
      binding.child?.run_id!==Number(env.GITHUB_RUN_ID)||binding.child.run_attempt!==1||binding.child.head_sha!==env.GITHUB_SHA||
      binding.child.workflow!=='.github/workflows/guard-release.yml'||binding.child.repository!==env.GITHUB_REPOSITORY||
      binding.slot?.group!=='techstack-service'||binding.slot.artifact!=='runtime-service'||binding.slot.operation!=='publish'||binding.slot.step_index!==0||
      !/^sha256:[a-f0-9]{64}$/.test(value.binding_digest??''))throw new Error('guard_native_build_authority_denied');
  const subjects=Object.fromEntries(['techstack-linux-amd64','techstack-linux-arm64','stackkit-runtime-linux-amd64.tar.gz','stackkit-runtime-linux-arm64.tar.gz'].map(name=>{
    const file=path.join(directory,name);return [name,{sha256:createHash('sha256').update(readFileSync(file)).digest('hex'),size:statSync(file).size}];
  }));
  return {schema:'guard.native-build/v1',sourceSha:source,sourceTree:execFileSync('git',['rev-parse','HEAD^{tree}'],{encoding:'utf8'}).trim(),
    executorSha:env.GITHUB_SHA,executorWorkflow:env.GITHUB_WORKFLOW_REF,parentRunId:binding.parent.run_id,parentAttempt:binding.parent.run_attempt,
    planDigest:binding.plan.digest,planArtifactId:binding.plan.artifact_id,planArchiveDigest:binding.plan.archive_digest,planArtifactName:binding.plan.artifact_name,
    compilerRef:binding.plan.compiler_ref,childRunId:binding.child.run_id,childAttempt:1,bindingName:binding.name,bindingDigest:value.binding_digest,subjects};
}
if(process.argv[1]&&path.resolve(process.argv[1])===fileURLToPath(import.meta.url))process.stdout.write(JSON.stringify(guardBuildProvenance(process.argv[2]))+'\n');
