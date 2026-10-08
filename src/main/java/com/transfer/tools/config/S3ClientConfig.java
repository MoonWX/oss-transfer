package com.transfer.tools.config;

import com.amazonaws.ClientConfiguration;
import com.amazonaws.auth.AWSStaticCredentialsProvider;
import com.amazonaws.auth.BasicAWSCredentials;
import com.amazonaws.client.builder.AwsClientBuilder;
import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.AmazonS3ClientBuilder;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

import javax.net.ssl.*;
import java.security.cert.X509Certificate;

@Configuration
public class S3ClientConfig {

    @Autowired
    private SourceBucketConfig sourceBucketConfig;

    @Autowired
    private DestBucketConfig destBucketConfig;

    @Autowired
    private ConnectionPoolConfig connectionPoolConfig;

    static {
        try {
            TrustManager[] trustAllCerts = new TrustManager[] {
                new X509TrustManager() {
                    public X509Certificate[] getAcceptedIssuers() {
                        return null;
                    }
                    public void checkClientTrusted(X509Certificate[] certs, String authType) {
                    }
                    public void checkServerTrusted(X509Certificate[] certs, String authType) {
                    }
                }
            };

            SSLContext sc = SSLContext.getInstance("SSL");
            sc.init(null, trustAllCerts, new java.security.SecureRandom());
            HttpsURLConnection.setDefaultSSLSocketFactory(sc.getSocketFactory());

            HostnameVerifier allHostsValid = (hostname, session) -> true;
            HttpsURLConnection.setDefaultHostnameVerifier(allHostsValid);
        } catch (Exception e) {
            e.printStackTrace();
        }
    }

    private AmazonS3 createS3Client(String endpoint, String ak, String sk) {
        BasicAWSCredentials credentials = new BasicAWSCredentials(ak, sk);
        ClientConfiguration clientConfig = new ClientConfiguration();
        clientConfig.setMaxConnections(connectionPoolConfig.getMaxConnections());
        clientConfig.setConnectionTimeout(connectionPoolConfig.getConnectionTimeout());
        clientConfig.setSocketTimeout(connectionPoolConfig.getSocketTimeout());
        clientConfig.setConnectionMaxIdleMillis(connectionPoolConfig.getConnectionMaxIdleMillis());
        clientConfig.setUseExpectContinue(connectionPoolConfig.getUseExpectContinue());
        clientConfig.setUseReaper(connectionPoolConfig.getUseReaper());
        clientConfig.setMaxErrorRetry(connectionPoolConfig.getMaxErrorRetry());
        clientConfig.setSignerOverride("S3SignerType");

        return AmazonS3ClientBuilder.standard()
                .withCredentials(new AWSStaticCredentialsProvider(credentials))
                .withEndpointConfiguration(
                    new AwsClientBuilder.EndpointConfiguration(endpoint, "default"))
                .withClientConfiguration(clientConfig)
                .withPathStyleAccessEnabled(true)
                .disableChunkedEncoding()
                .build();
    }
}
