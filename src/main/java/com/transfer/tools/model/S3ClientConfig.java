package com.transfer.tools.model;

import com.amazonaws.auth.AWSCredentials;
import com.amazonaws.auth.BasicAWSCredentials;
import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class S3ClientConfig {
    private String alias;
    private String endpoint;
    private String accessKey;
    private String secretKey;
    private Boolean useV2SignerType;
    private Boolean pathStyleEnable;

    public AWSCredentials getCredentials() {
        return new BasicAWSCredentials(accessKey, secretKey);
    }
}
