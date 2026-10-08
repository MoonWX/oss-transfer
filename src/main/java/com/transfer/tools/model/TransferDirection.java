package com.transfer.tools.model;

public enum TransferDirection {
    OSS_TO_OSS(EndpointType.OSS, EndpointType.OSS),
    OSS_TO_LOCAL(EndpointType.OSS, EndpointType.LOCAL),
    LOCAL_TO_OSS(EndpointType.LOCAL, EndpointType.OSS);

    private final EndpointType sourceType;
    private final EndpointType destinationType;

    TransferDirection(EndpointType sourceType, EndpointType destinationType) {
        this.sourceType = sourceType;
        this.destinationType = destinationType;
    }

    public EndpointType getSourceType() {
        return sourceType;
    }

    public EndpointType getDestinationType() {
        return destinationType;
    }
}
