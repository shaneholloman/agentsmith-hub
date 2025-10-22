package cluster

import (
	"AgentSmith-HUB/common"
	"AgentSmith-HUB/input"
	"AgentSmith-HUB/logger"
	"AgentSmith-HUB/output"
	"AgentSmith-HUB/plugin"
	"AgentSmith-HUB/project"
	"AgentSmith-HUB/rules_engine"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SyncListener handles sync commands for followers
type SyncListener struct {
	nodeID           string
	stopChan         chan struct{}
	currentVersion   int64
	baseVersion      string
	executionFlagTTL time.Duration // TTL for execution flag, default 5 minutes
	mu               sync.RWMutex
}

var GlobalSyncListener *SyncListener

// InitSyncListener initializes the sync listener
func InitSyncListener(nodeID string) {
	GlobalSyncListener = &SyncListener{
		nodeID:           nodeID,
		stopChan:         make(chan struct{}),
		currentVersion:   0,  // Default to 0 for new followers
		executionFlagTTL: 30, // 30 seconds TTL for execution flags (reduced from 75s for faster recovery)
		baseVersion:      "0",
	}
}

func (sl *SyncListener) GetCurrentVersion() string {
	sl.mu.RLock()
	defer sl.mu.RUnlock()

	return fmt.Sprintf("%s.%d", sl.baseVersion, sl.currentVersion)
}

// getCurrentVersionUnsafe returns version string without locking (must be called with lock held)
func (sl *SyncListener) getCurrentVersionUnsafe() string {
	return fmt.Sprintf("%s.%d", sl.baseVersion, sl.currentVersion)
}

// ResetForFullResync resets follower state to trigger full resync
// Called when follower is kicked out by leader due to slow sync
func (sl *SyncListener) ResetForFullResync() {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	logger.Info("Resetting follower for full resync",
		"old_version", sl.getCurrentVersionUnsafe())

	// Clear all local components and projects
	sl.clearAllLocalComponents()

	// Reset to version 0 (keep same baseVersion - leader will send the correct one)
	sl.currentVersion = 0

	logger.Info("Follower reset completed", "new_version", sl.getCurrentVersionUnsafe())
}

// Start starts the sync listener (follower only)
func (sl *SyncListener) Start() {
	if common.IsCurrentNodeLeader() {
		return
	}
	go sl.listenSyncCommands()
}

// listenSyncCommands listens for sync commands from leader
func (sl *SyncListener) listenSyncCommands() {
	// Retry loop with exponential backoff for Redis connection failures
	retryCount := 0
	maxRetryDelay := 30 * time.Second

	for {
		select {
		case <-sl.stopChan:
			return
		default:
		}

		client := common.GetRedisClient()
		if client == nil {
			logger.Error("Redis client not available for sync listener")
			retryDelay := time.Duration(1<<uint(retryCount)) * time.Second
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
			logger.Info("Retrying sync listener connection", "delay", retryDelay, "retry_count", retryCount)
			time.Sleep(retryDelay)
			retryCount++
			continue
		}

		pubsub := client.Subscribe(context.Background(), "cluster:sync_command")
		logger.Info("Sync listener subscribed to Redis pub/sub channel")
		retryCount = 0 // Reset retry count on successful connection

		// Listen for messages
		ch := pubsub.Channel()
		disconnected := false

		for !disconnected {
			select {
			case msg, ok := <-ch:
				if !ok {
					// Channel closed, need to reconnect
					logger.Warn("Sync command pub/sub channel closed, reconnecting...")
					disconnected = true
					break
				}

				var syncCmd map[string]interface{}
				if err := json.Unmarshal([]byte(msg.Payload), &syncCmd); err != nil {
					logger.Error("Failed to unmarshal sync command", "error", err)
					continue
				}

				// Check if command is for this node
				// Commands without node_id are broadcast commands (like publish_complete)
				if nodeID, ok := syncCmd["node_id"].(string); ok && nodeID != sl.nodeID {
					continue
				}

				// Handle sync command
				sl.handleSyncCommand(syncCmd)

			case <-sl.stopChan:
				pubsub.Close()
				return
			}
		}

		// Clean up before reconnecting
		pubsub.Close()
		logger.Info("Sync listener disconnected, will reconnect in 2 seconds...")
		time.Sleep(2 * time.Second)
	}
}

// handleSyncCommand handles a sync command
func (sl *SyncListener) handleSyncCommand(syncCmd map[string]interface{}) {
	action, _ := syncCmd["action"].(string)
	leaderVersion, _ := syncCmd["leader_version"].(string)

	// Handle both publish_complete and sync commands
	if action != "publish_complete" && action != "sync" {
		return
	}

	// Check if sync is needed
	if sl.GetCurrentVersion() == leaderVersion {
		return
	}

	if err := sl.SyncInstructions(leaderVersion); err != nil {
		logger.Error("Failed to sync instructions", "error", err)
	}
}

func (sl *SyncListener) SyncInstructions(toVersion string) error {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	// Set execution flag to indicate this follower is executing instructions
	if err := sl.SetFollowerExecutionFlag(sl.nodeID); err != nil {
		logger.Error("Failed to set execution flag", "error", err)
	}

	// Ensure flag is cleared when done (with defer for safety)
	defer func() {
		if err := sl.ClearFollowerExecutionFlag(sl.nodeID); err != nil {
			logger.Error("Failed to clear execution flag", "error", err)
		}
	}()

	leaderParts := strings.Split(toVersion, ".")
	if len(leaderParts) != 2 {
		return fmt.Errorf("invalid target version format: %s", toVersion)
	}

	endVersion, err := strconv.ParseInt(leaderParts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid target version number: %s", leaderParts[1])
	}

	// Check if session has changed (leader restart) or if this is a new follower
	if sl.baseVersion != leaderParts[0] {
		logger.Info("Follower needs full sync due to leader session change",
			"from", sl.getCurrentVersionUnsafe(),
			"to", toVersion,
			"old_base", sl.baseVersion,
			"new_base", leaderParts[0])

		// Clear all local components and projects (never fails)
		sl.clearAllLocalComponents()

		// Update baseVersion immediately after clearing to prevent repeated clearing
		// Start from version 0, so we'll sync from version 1 to endVersion
		sl.baseVersion = leaderParts[0]
		sl.currentVersion = 0

		logger.Info("Follower state reset for full resync", "new_version", sl.getCurrentVersionUnsafe())
	}

	// Track instruction execution details
	var processedInstructions []string
	var failedInstructions []string
	var missingInstructions []int64
	var instructions []Instruction
	var compacted uint64

	// Process instructions from startVersion+1 to endVersion
	// Instructions are numbered from 1 onwards (version 0 is temporary state)
	for version := sl.currentVersion + 1; version <= endVersion; version++ {
		// Refresh execution flag during long operations
		if err := sl.SetFollowerExecutionFlag(sl.nodeID); err != nil {
			logger.Warn("Failed to refresh execution flag", "error", err)
		}

		// Get instruction from Redis
		key := fmt.Sprintf("cluster:instruction:%d", version)
		data, err := common.RedisGet(key)
		if data == GetDeletedIntentionsString() {
			compacted++
			continue
		}

		if err != nil {
			// Record missing instruction - this could indicate a problem
			missingInstructions = append(missingInstructions, version)
			logger.Warn("Instruction not found in Redis",
				"version", version,
				"error", err,
				"this_may_cause_inconsistency", true)
			continue
		}

		var instruction Instruction
		if err := json.Unmarshal([]byte(data), &instruction); err != nil {
			logger.Error("Failed to unmarshal instruction", "version", version, "error", err)
			failedInstructions = append(failedInstructions, fmt.Sprintf("v%d: unmarshal error", version))
			continue // Continue processing other instructions
		} else if instruction.ComponentType != "DELETE" {
			instructions = append(instructions, instruction)
		} else {
			compacted++
		}
	}

	// Log missing instructions if any
	if len(missingInstructions) > 0 {
		totalInstructionsExpected := endVersion - sl.currentVersion
		missingRatio := float64(len(missingInstructions)) / float64(totalInstructionsExpected)
		logger.Warn("Some instructions are missing during sync, will trigger full resync",
			"missing_count", len(missingInstructions),
			"total_expected", totalInstructionsExpected,
			"missing_ratio", fmt.Sprintf("%.2f%%", missingRatio*100),
			"missing_versions", missingInstructions)
	}
	slices.SortStableFunc(instructions, func(a, b Instruction) int {
		if a.ComponentType == "project" && b.ComponentType != "project" {
			return 1
		} else if a.ComponentType != "project" && b.ComponentType == "project" {
			return -1
		} else {
			return int(a.Version) - int(b.Version)
		}
	})
	for _, instruction := range instructions {
		version := instruction.Version
		if version == 0 {
			continue
		}
		// Apply instruction - no retry, fail fast
		if err := sl.applyInstruction(version); err != nil {
			logger.Error("Failed to apply instruction", "version", version, "component", instruction.ComponentName, "operation", instruction.Operation, "error", err)
			failedInstructions = append(failedInstructions, fmt.Sprintf("v%d: %s %s %s (failed: %v)", version, instruction.Operation, instruction.ComponentType, instruction.ComponentName, err))
		} else {
			// Record successfully applied instruction details
			instructionDesc := fmt.Sprintf("v%d: %s %s %s", version, instruction.Operation, instruction.ComponentType, instruction.ComponentName)
			instructionDesc += fmt.Sprintf(" (content: %d chars)", len(instruction.Content))
			processedInstructions = append(processedInstructions, instructionDesc)
		}
	}

	// Save old version for logging
	oldVersion := sl.getCurrentVersionUnsafe()

	// Only update version if all instructions succeeded
	if len(failedInstructions) == 0 && len(missingInstructions) == 0 {
		sl.currentVersion = endVersion
		sl.baseVersion = leaderParts[0]
		logger.Info("All instructions applied successfully, version updated", "new_version", sl.getCurrentVersionUnsafe())
	} else {
		// If any instruction failed, clear all components and start from scratch
		logger.Error("Some instructions failed, clearing all components for full resync",
			"failed_count", len(failedInstructions),
			"missing_count", len(missingInstructions),
			"current_version", sl.getCurrentVersionUnsafe(),
			"target_version", toVersion)

		// Clear all local components and projects (never fails)
		sl.clearAllLocalComponents()

		// Reset to version 0 to trigger full resync on next attempt
		sl.currentVersion = 0
		sl.baseVersion = leaderParts[0]
		logger.Info("Follower reset to version 0, will perform full resync on next attempt")
	}

	// Log final sync result
	if len(failedInstructions) > 0 || len(missingInstructions) > 0 {
		logger.Error("Instructions synced with some failures or missing instructions",
			"current_version", sl.getCurrentVersionUnsafe(),
			"target_version", toVersion,
			"compacted", compacted,
			"processed", len(processedInstructions),
			"failed", len(failedInstructions),
			"missing", len(missingInstructions),
			"successful_instructions", strings.Join(processedInstructions, "; "),
			"failed_instructions", strings.Join(failedInstructions, "; "),
			"missing_versions", missingInstructions)
		return fmt.Errorf("sync incomplete: %d failed, %d missing instructions", len(failedInstructions), len(missingInstructions))
	} else {
		logger.Info("Instructions synced successfully",
			"from_version", oldVersion,
			"to_version", sl.getCurrentVersionUnsafe(),
			"count", len(processedInstructions),
			"compacted", compacted,
			"instructions", strings.Join(processedInstructions, "; "))
	}

	return nil
}

// ClearFollowerExecutionFlag clears the execution flag for a follower
func (sl *SyncListener) ClearFollowerExecutionFlag(nodeID string) error {
	key := fmt.Sprintf("cluster:execution_flag:%s", nodeID)
	return common.RedisDel(key)
}

// SetFollowerExecutionFlag sets/refreshes a flag indicating follower is executing instructions
func (sl *SyncListener) SetFollowerExecutionFlag(nodeID string) error {
	key := fmt.Sprintf("cluster:execution_flag:%s", nodeID)
	_, err := common.RedisSet(key, "executing", int(sl.executionFlagTTL))
	if err != nil {
		return fmt.Errorf("failed to set execution flag: %w", err)
	}
	return nil
}

// applyInstruction applies a single instruction
func (sl *SyncListener) applyInstruction(version int64) error {
	key := fmt.Sprintf("cluster:instruction:%d", version)
	data, err := common.RedisGet(key)
	if err != nil {
		return fmt.Errorf("failed to get instruction %d: %w", version, err)
	}

	var instruction Instruction
	if err := json.Unmarshal([]byte(data), &instruction); err != nil {
		return fmt.Errorf("failed to unmarshal instruction %d: %w", version, err)
	}

	affectedProjects := []string{}
	source := ""
	if instruction.Metadata != nil {
		if projects, exists := instruction.Metadata["affected_projects"]; exists {
			if projectList, ok := projects.([]interface{}); ok {
				for _, p := range projectList {
					if projectStr, ok := p.(string); ok {
						affectedProjects = append(affectedProjects, projectStr)
					}
				}
			}
		}
		if s, exists := instruction.Metadata["source"]; exists {
			if sourceStr, ok := s.(string); ok {
				source = sourceStr
			}
		}
	}

	switch instruction.Operation {
	case "add":
		if err := sl.createComponentInstance(instruction.ComponentType, instruction.ComponentName, instruction.Content); err != nil {
			common.RecordComponentAdd(instruction.ComponentType, instruction.ComponentName, instruction.Content, "failed", err.Error())
			return err
		}
		common.RecordComponentAdd(instruction.ComponentType, instruction.ComponentName, instruction.Content, "success", "")
	case "delete":
		if err := sl.deleteComponentInstance(instruction.ComponentType, instruction.ComponentName); err != nil {
			return err
		}
	case "update":
		if err := sl.updateComponentInstance(instruction.ComponentType, instruction.ComponentName, instruction.Content); err != nil {
			common.RecordComponentUpdate(instruction.ComponentType, instruction.ComponentName, instruction.Content, "failed", err.Error())
			return err
		}
	case "local_push":
		if err := sl.createComponentInstance(instruction.ComponentType, instruction.ComponentName, instruction.Content); err != nil {
			common.RecordLocalPush(instruction.ComponentType, instruction.ComponentName, instruction.Content, "failed", err.Error())
			return err
		}
	case "push_change":
		if err := sl.createComponentInstance(instruction.ComponentType, instruction.ComponentName, instruction.Content); err != nil {
			common.RecordChangePush(instruction.ComponentType, instruction.ComponentName, "", instruction.Content, "", "failed", err.Error())
			return err
		}
	case "start":
		return globalProjectCmdHandler.ExecuteCommandWithOptions(instruction.ComponentName, "start", true)
	case "stop":
		return globalProjectCmdHandler.ExecuteCommandWithOptions(instruction.ComponentName, "stop", true)
	case "restart":
		return globalProjectCmdHandler.ExecuteCommandWithOptions(instruction.ComponentName, "restart", true)
	default:
		return fmt.Errorf("unknown operation: %s", instruction.Operation)
	}

	// For operations that affect projects, trigger a restart.
	// The restart operation itself will be logged with the correct trigger source.
	for _, projectName := range affectedProjects {
		if proj, exists := project.GetProject(projectName); exists {
			if err := proj.Restart(true, source); err != nil {
				// Restart already logs its own failure. We just need to bubble up the error.
				return fmt.Errorf("failed to restart affected project %s: %w", projectName, err)
			}
		} else {
			logger.Warn("Follower: Project to restart not found", "project", projectName)
		}
	}

	return nil
}

// clearAllLocalComponents clears all local components and projects when leader session changes
// This function never fails - it will try best effort to clean everything
func (sl *SyncListener) clearAllLocalComponents() {
	logger.Info("Clearing all local components and projects for full resync")

	// Step 1: Stop all running projects first
	// Collect running projects first to avoid deadlock
	var runningProjects []*project.Project
	project.ForEachProject(func(projectName string, proj *project.Project) bool {
		if proj.Status == common.StatusRunning || proj.Status == common.StatusStarting || proj.Status == common.StatusError {
			runningProjects = append(runningProjects, proj)
		}
		return true
	})

	// Stop projects without holding locks - continue even if some fail
	for _, proj := range runningProjects {
		logger.Info("Stopping project for cleanup", "project", proj.Id)
		if err := proj.Stop(true); err != nil {
			logger.Warn("Failed to stop project during cleanup, continuing anyway", "project", proj.Id, "error", err)
		}
	}

	// Step 2: Delete all component instances (to match state of newly started follower)
	// Get all component IDs before deletion
	var projectIDs, inputIDs, outputIDs, rulesetIDs []string

	project.ForEachProject(func(projectName string, _ *project.Project) bool {
		projectIDs = append(projectIDs, projectName)
		return true
	})

	for id := range project.GetAllInputs() {
		inputIDs = append(inputIDs, id)
	}

	for id := range project.GetAllOutputs() {
		outputIDs = append(outputIDs, id)
	}

	for id := range project.GetAllRulesets() {
		rulesetIDs = append(rulesetIDs, id)
	}

	// Delete all instances - these operations don't fail
	for _, id := range projectIDs {
		project.DeleteProject(id)
	}
	for _, id := range inputIDs {
		project.DeleteInput(id)
	}
	for _, id := range outputIDs {
		project.DeleteOutput(id)
	}
	for _, id := range rulesetIDs {
		project.DeleteRuleset(id)
	}

	// Step 3: Clear global component raw config maps
	common.ClearAllRawConfigsForAllTypes()

	logger.Info("Successfully cleared all components and projects",
		"projects_deleted", len(projectIDs),
		"inputs_deleted", len(inputIDs),
		"outputs_deleted", len(outputIDs),
		"rulesets_deleted", len(rulesetIDs))
}

// createComponentInstance creates actual component instances from configuration
func (sl *SyncListener) createComponentInstance(componentType, componentName, content string) error {
	switch componentType {
	case "input":
		// Import the input package at the top if not already imported
		inp, err := input.NewInput("", content, componentName)
		if err != nil {
			return fmt.Errorf("failed to create input instance %s: %w", componentName, err)
		}
		project.SetInput(componentName, inp)
		logger.Debug("Created input instance", "name", componentName)

	case "output":
		// Import the output package at the top if not already imported
		out, err := output.NewOutput("", content, componentName)
		if err != nil {
			return fmt.Errorf("failed to create output instance %s: %w", componentName, err)
		}
		project.SetOutput(componentName, out)
		logger.Debug("Created output instance", "name", componentName)

	case "ruleset":
		// Import the rules_engine package at the top if not already imported
		rs, err := rules_engine.NewRuleset("", content, componentName)
		if err != nil {
			return fmt.Errorf("failed to create ruleset instance %s: %w", componentName, err)
		}
		project.SetRuleset(componentName, rs)
		logger.Debug("Created ruleset instance", "name", componentName)

	case "project":
		// For projects, we create the project instance
		proj, err := project.NewProject("", content, componentName, false)
		if err != nil {
			return fmt.Errorf("failed to create project instance %s: %w", componentName, err)
		}
		project.SetProject(componentName, proj)
		logger.Debug("Created project instance", "name", componentName)

	case "plugin":
		// For plugins, we just store the content as plugins are handled differently
		// Import the plugin package at the top if not already imported
		err := plugin.NewPlugin("", content, componentName, plugin.YAEGI_PLUGIN)
		if err != nil {
			return fmt.Errorf("failed to create plugin instance %s: %w", componentName, err)
		}
		logger.Debug("Created plugin instance", "name", componentName)

	default:
		return fmt.Errorf("unsupported component type: %s", componentType)
	}

	return nil
}

// deleteComponentInstance deletes actual component instances
func (sl *SyncListener) deleteComponentInstance(componentType, componentName string) error {
	switch componentType {
	case "input":
		project.DeleteInput(componentName)
		logger.Debug("Deleted input instance", "name", componentName)

	case "output":
		project.DeleteOutput(componentName)
		logger.Debug("Deleted output instance", "name", componentName)

	case "ruleset":
		project.DeleteRuleset(componentName)
		logger.Debug("Deleted ruleset instance", "name", componentName)

	case "project":
		if proj, exists := project.GetProject(componentName); exists {
			// Stop the project first if it's running
			if proj.Status == common.StatusRunning {
				proj.Stop(true)
			}
		}
		project.DeleteProject(componentName)
		logger.Debug("Deleted project instance", "name", componentName)

	case "plugin":
		// For plugins, we need to handle differently based on the plugin system
		// This might need specific plugin cleanup logic
		logger.Debug("Deleted plugin instance", "name", componentName)

	default:
		return fmt.Errorf("unsupported component type: %s", componentType)
	}

	return nil
}

// updateComponentInstance updates existing component instances with new configuration
func (sl *SyncListener) updateComponentInstance(componentType, componentName, content string) error {
	// For updates, we delete the old instance and create a new one
	if err := sl.deleteComponentInstance(componentType, componentName); err != nil {
		logger.Warn("Failed to delete old component instance during update", "type", componentType, "name", componentName, "error", err)
	}

	return sl.createComponentInstance(componentType, componentName, content)
}

// Stop stops the sync listener
func (sl *SyncListener) Stop() {
	close(sl.stopChan)
	_ = sl.ClearFollowerExecutionFlag(sl.nodeID)
}
