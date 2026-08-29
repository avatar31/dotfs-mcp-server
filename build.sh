#!/bin/bash

export DOTFS_WORKSPACE_ROOT=$HOME/code/dotfs-workspace
export DOTFS_SKIP_DIRS=".git,.svn,.hg,node_modules,vendor,third_party,build,dist,out,.idea,.vscode,dotfs-mcp-server,wireforge,graphify-out"

pwd=$(pwd)

echo "Building dotfs-mcp-server..."
echo $pwd

make run
