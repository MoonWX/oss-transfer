package com.transfer.tools.agent;

import java.io.IOException;

/** A request rejected before delivery has no ambiguous remote mutation to replay. */
public class AgentUnavailableException extends IOException {
    public AgentUnavailableException(String message) { super(message); }
}
